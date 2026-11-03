// Command quesma-unseal is a custodian's tool for a bucket archive copied locally by rclone: open
// decrypts it into a readable tree, PLAIN_DIR/<install>/<source_id>/<native_path> plus a
// .manifest.json sidecar per object, and reseal writes each object sealed to one more recipient
// under its own key, for rclone to upload. The archive is written by developer machines and
// untrusted, so every write is confined by os.Root.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"filippo.io/age"
	"golang.org/x/text/unicode/norm"

	"github.com/QuesmaOrg/quesma-shipper/internal/legal"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

const usage = `usage: quesma-unseal open -i IDENTITY_FILE [-i IDENTITY_FILE]... ENC_DIR PLAIN_DIR
       quesma-unseal reseal -i IDENTITY_FILE [-i IDENTITY_FILE]... -to RECIPIENT [-to RECIPIENT]... ENC_DIR OUT_DIR
       quesma-unseal version
       quesma-unseal licenses`

// releaseVersion is the release this binary was built for (-X main.releaseVersion): objects from a
// newer shipper may carry a manifest version it refuses.
var releaseVersion = "dev"

const sidecarSuffix = ".manifest.json"

// indexName maps each object's key to the output it last wrote, so an object whose native path
// moved, as a Codex session does when archived, does not leave its earlier copy behind.
const indexName = ".unseal-index.json"

// stagingDir holds every file before its rename into place. A run removes it first and last, so an
// interrupted run leaves nothing partial beside an output.
const stagingDir = ".unseal-tmp"

// maxTagsBytes bounds tags.json, which holds a name and a few labels; a huge one is hostile.
const maxTagsBytes = 1 << 20

// maxNameBytes is NAME_MAX on APFS and ext4.
const maxNameBytes = 255

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "open", "reseal":
		return runCommand(cmd, args, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "quesma-unseal %s\n", releaseVersion)
		return 0
	case "licenses":
		if err := legal.Write(stdout); err != nil {
			fmt.Fprintf(stderr, "quesma-unseal: %v\n", err)
			return 1
		}
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(stdout, usage)
		return 0
	case "":
		fmt.Fprintln(stderr, usage)
	default:
		// Before the commands, the flags came first; scripts copied from that form still exist.
		if strings.HasPrefix(cmd, "-") {
			fmt.Fprintf(stderr, "quesma-unseal: the flags follow a command now: quesma-unseal open %s ..., and -reseal-to R is reseal -to R\n%s\n", cmd, usage)
			return 2
		}
		fmt.Fprintf(stderr, "quesma-unseal: unknown command %q\n%s\n", cmd, usage)
	}
	return 2
}

func runCommand(cmd string, args []string, stdout, stderr io.Writer) int {
	start := time.Now()
	reseal := cmd == "reseal"
	flags := flag.NewFlagSet("quesma-unseal "+cmd, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var idFiles []string
	flags.Func("i", "age identity file of AGE-SECRET-KEY-1 lines, as age-keygen writes (repeatable)", func(v string) error {
		idFiles = append(idFiles, v)
		return nil
	})
	var resealTo []string
	if reseal {
		flags.Func("to", "age recipient to add to every object not sealed to it (repeatable)", func(v string) error {
			r, err := age.ParseX25519Recipient(v)
			if err == nil {
				resealTo = append(resealTo, r.String())
			}
			return err
		})
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "quesma-unseal: %v\n%s\n", err, usage)
		return 2
	}
	if flags.NArg() != 2 || len(idFiles) == 0 || reseal && len(resealTo) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	ids, err := loadIdentities(idFiles)
	if err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: %v\n", err)
		return 2
	}
	enc, err := os.OpenRoot(flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: ENC_DIR: %v\n", err)
		return 2
	}
	defer enc.Close()
	outName, work, done, kept := "PLAIN_DIR", (*unsealer).object, "decrypted", "unchanged"
	if reseal {
		outName, work, done, kept = "OUT_DIR", (*unsealer).reseal, "resealed", "already sealed"
	}
	plain, err := openPlainDir(flags.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: %s: %v\n", outName, err)
		return 2
	}
	defer plain.Close()
	if err := plain.RemoveAll(stagingDir); err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: %s: %v\n", outName, err)
		return 2
	}
	if err := plain.Mkdir(stagingDir, 0o700); err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: %s: %v\n", outName, err)
		return 2
	}
	defer plain.RemoveAll(stagingDir)

	u := &unsealer{
		enc: enc, plain: plain, ids: ids, resealTo: resealTo, stderr: stderr,
		installDirs: map[string]string{}, claimed: map[string]string{},
	}
	if u.foldCase, u.foldNFC, err = probeFolding(plain); err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: %s: %v\n", outName, err)
		return 2
	}
	if !reseal {
		u.loadIndex()
	}
	objects := u.collect()
	if len(objects) == 0 {
		fmt.Fprintf(stderr, "warning: no objects under %s; it should be a copy of v1/organization=<org>\n", flags.Arg(0))
	}
	for _, o := range objects {
		decrypted, err := work(u, o)
		switch {
		case err != nil:
			u.fail(o.rel, err)
		case decrypted:
			u.decrypted++
		default:
			u.unchanged++
		}
	}
	if u.index != nil {
		if err := u.saveIndex(); err != nil {
			u.fail(indexName, err)
		}
	}
	fmt.Fprintf(stdout, "%s %d, %s %d, failed %d in %.1fs\n",
		done, u.decrypted, kept, u.unchanged, u.failed, time.Since(start).Seconds())
	if u.failed > 0 {
		return 1
	}
	return 0
}

// openPlainDir creates PLAIN_DIR (parent must exist) through os.Root like every other write.
func openPlainDir(dir string) (*os.Root, error) {
	dir = filepath.Clean(dir)
	parent, err := os.OpenRoot(filepath.Dir(dir))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err := parent.Mkdir(filepath.Base(dir), 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return os.OpenRoot(dir)
}

func loadIdentities(files []string) ([]age.Identity, error) {
	var ids []age.Identity
	for _, name := range files {
		f, err := os.Open(name)
		if err != nil {
			return nil, fmt.Errorf("identity file: %w", err)
		}
		parsed, err := age.ParseIdentities(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("identity file %s: %w", name, err)
		}
		ids = append(ids, parsed...)
	}
	return ids, nil
}

type unsealer struct {
	enc, plain *os.Root
	ids        []age.Identity
	resealTo   []string
	stderr     io.Writer

	installDirs map[string]string // install dir rel path -> output directory name
	claimed     map[string]string // output path, folded as PLAIN_DIR folds names -> .age rel path that claimed it
	foldCase    bool              // PLAIN_DIR's filesystem treats names differing only in case as one
	foldNFC     bool              // and names differing only in Unicode normalization
	index       map[string]string // object key from install= on -> output path; nil under -reseal-to

	decrypted, unchanged, failed int // under -reseal-to: resealed, already sealed, failed
}

// object is one mirror .age file, with the ids its key claims for it.
type object struct {
	rel, key, installRel, installID, sourceID string
}

func (u *unsealer) fail(rel string, err error) {
	u.failed++
	fmt.Fprintf(u.stderr, "unseal %s: %s\n", oneLine(rel), oneLine(err.Error()))
}

// collect walks ENC_DIR lexically, finding install=<id> at any depth. Object storage has no
// symlinks or FIFOs, so one in an object's place was put there locally: it fails, never followed.
func (u *unsealer) collect() []object {
	var objects []object
	_ = fs.WalkDir(u.enc.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			u.fail(p, err)
			return nil
		}
		segs := strings.Split(p, "/")
		if !d.IsDir() && tailMatches(segs, objectLayout) {
			// Opening a FIFO blocks until a writer appears, so it must never reach object.
			if !d.Type().IsRegular() {
				u.fail(p, errors.New("not a regular file"))
				return nil
			}
			n := len(segs)
			objects = append(objects, object{
				rel:        p,
				key:        strings.Join(segs[n-4:], "/"),
				installRel: strings.Join(segs[:n-3], "/"),
				installID:  strings.TrimPrefix(segs[n-4], "install="),
				sourceID:   strings.TrimPrefix(segs[n-2], "source="),
			})
		}
		return nil
	})
	return objects
}

// objectLayout is the archive layout under an install, as one path.Match pattern per segment.
var objectLayout = []string{"install=*", "mirror", "source=*", "*.age"}

func tailMatches(segs, pattern []string) bool {
	if len(segs) < len(pattern) {
		return false
	}
	tail := segs[len(segs)-len(pattern):]
	for i, pat := range pattern {
		if ok, _ := path.Match(pat, tail[i]); !ok {
			return false
		}
	}
	return true
}

// object brings one output up to date and reports whether it decrypted the payload.
func (u *unsealer) object(o object) (bool, error) {
	f, info, err := u.openObject(o)
	if err != nil {
		return false, err
	}
	defer f.Close()
	obj := io.NewSectionReader(f, 0, info.Size())
	m, err := transforms.ReadManifest(io.NewSectionReader(obj, 0, obj.Size()), u.ids...)
	if err != nil {
		return false, err
	}

	out, err := u.outputPath(o, m)
	if err != nil {
		return false, err
	}
	if out, err = u.claim(o, out); err != nil {
		return false, err
	}

	ok, err := u.upToDate(out, m)
	if err != nil {
		return false, err
	}
	if !ok {
		if err := u.write(obj, out, info.ModTime()); err != nil {
			return false, err
		}
	}
	if prev := u.index[o.key]; prev != "" && u.fold(prev) != u.fold(out) {
		u.removeSuperseded(o, prev)
	}
	u.index[o.key] = out
	return !ok, nil
}

// removeSuperseded deletes the output o wrote before its native path moved, unless another object
// holds that path this run or it is not an output, a payload with its sidecar beside it.
func (u *unsealer) removeSuperseded(o object, prev string) {
	if holder, ok := u.claimedBy(prev); ok && holder != o.rel {
		return
	}
	for _, p := range []string{prev, prev + sidecarSuffix} {
		if info, err := u.plain.Lstat(p); err != nil || !info.Mode().IsRegular() {
			return
		}
	}
	for _, p := range []string{prev, prev + sidecarSuffix} {
		if err := u.plain.Remove(p); err != nil {
			fmt.Fprintf(u.stderr, "warning %s: removing %s: %s\n", oneLine(o.rel), oneLine(p), oneLine(err.Error()))
			return
		}
	}
}

// loadIndex starts from an empty index when there is none; an unreadable one only costs this run
// its removals.
func (u *unsealer) loadIndex() {
	u.index = map[string]string{}
	raw, err := u.plain.ReadFile(indexName)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err == nil {
		err = json.Unmarshal(raw, &u.index)
	}
	if err != nil {
		u.index = map[string]string{}
		fmt.Fprintf(u.stderr, "warning %s: %s; outputs whose object moved are kept this run\n", indexName, oneLine(err.Error()))
	}
}

func (u *unsealer) saveIndex() error {
	raw, err := json.MarshalIndent(u.index, "", "  ")
	if err != nil {
		return err
	}
	return u.place(indexName, raw, time.Now())
}

// claim reserves out and its sidecar for o. Where PLAIN_DIR would merge them with another object's
// output, o is written beside it, named after its own blinded key, so both are kept.
func (u *unsealer) claim(o object, out string) (string, error) {
	prev, taken := u.claimedBy(out)
	if taken {
		alt := besideName(out, o.rel)
		if other, ok := u.claimedBy(alt); ok {
			return "", fmt.Errorf("output %s already written by %s, and %s by %s", out, prev, alt, other)
		}
		fmt.Fprintf(u.stderr, "warning %s: %s is %s's on this filesystem; writing %s\n",
			oneLine(o.rel), oneLine(out), oneLine(prev), oneLine(alt))
		out = alt
	}
	u.claimed[u.fold(out)] = o.rel
	u.claimed[u.fold(out+sidecarSuffix)] = o.rel
	return out, nil
}

func (u *unsealer) claimedBy(out string) (string, bool) {
	for _, p := range []string{out, out + sidecarSuffix} {
		if prev, ok := u.claimed[u.fold(p)]; ok {
			return prev, true
		}
	}
	return "", false
}

func (u *unsealer) fold(p string) string {
	if u.foldNFC {
		p = norm.NFC.String(p)
	}
	if u.foldCase {
		p = strings.ToLower(p)
	}
	return p
}

// besideName inserts up to 8 characters of the object's blinded name before out's extension.
func besideName(out, rel string) string {
	tag := strings.TrimSuffix(path.Base(rel), ".age")
	tag = tag[:min(8, len(tag))]
	ext := path.Ext(out)
	return strings.TrimSuffix(out, ext) + "~" + tag + ext
}

// probeFolding asks PLAIN_DIR's filesystem, in stagingDir, whether it merges names that differ only
// in case or in Unicode normalization. It is a variable so a test can play either filesystem.
var probeFolding = func(root *os.Root) (foldCase, foldNFC bool, err error) {
	id := tempName()
	for _, p := range []struct {
		create, look string
		fold         *bool
	}{
		{"x" + id, "X" + strings.ToUpper(id), &foldCase},
		{"e\u0301" + id, "\u00e9" + id, &foldNFC},
	} {
		f, err := root.Create(path.Join(stagingDir, p.create))
		if err != nil {
			return false, false, fmt.Errorf("name probe: %w", err)
		}
		f.Close()
		_, err = root.Stat(path.Join(stagingDir, p.look))
		*p.fold = err == nil
	}
	return foldCase, foldNFC, nil
}

func (u *unsealer) openObject(o object) (*os.File, fs.FileInfo, error) {
	f, err := u.enc.Open(o.rel)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// reseal writes o to its own path under OUT_DIR, sealed to the recipients its manifest lists plus
// any of resealTo it lacks, with the same payload and the same mtime, so rclone copy --update skips
// a key a shipper has rewritten since. The payload was scrubbed when it was first shipped.
func (u *unsealer) reseal(o object) (bool, error) {
	f, info, err := u.openObject(o)
	if err != nil {
		return false, err
	}
	defer f.Close()
	obj := io.NewSectionReader(f, 0, info.Size())
	m, err := transforms.ReadManifest(io.NewSectionReader(obj, 0, obj.Size()), u.ids...)
	if err != nil {
		return false, err
	}
	if err := keyMatches(o, m); err != nil {
		return false, err
	}
	if m.Encryption == nil || len(m.Encryption.RecipientKeyIDs) == 0 {
		return false, errors.New("manifest lists no recipients")
	}
	ids := slices.Clone(m.Encryption.RecipientKeyIDs)
	for _, r := range u.resealTo {
		if !slices.Contains(ids, r) {
			ids = append(ids, r)
		}
	}
	if len(ids) == len(m.Encryption.RecipientKeyIDs) {
		return false, nil
	}
	recipients := make([]age.Recipient, len(ids))
	for i, id := range ids {
		if recipients[i], err = age.ParseX25519Recipient(id); err != nil {
			return false, fmt.Errorf("manifest recipient %q: %w", id, err)
		}
	}
	var payload bytes.Buffer
	if m, err = transforms.OpenTo(io.NewSectionReader(obj, 0, obj.Size()), &payload, u.ids...); err != nil {
		return false, err
	}
	m.Encryption.RecipientKeyIDs = ids
	sealed, _, err := transforms.Seal(m, transforms.Unscrubbed(payload.Bytes(), "reseal of a shipped payload"), recipients)
	if err != nil {
		return false, err
	}
	return true, u.place(o.rel, sealed, info.ModTime())
}

// place writes body at rel under the output root through a staged rename.
func (u *unsealer) place(rel string, body []byte, mtime time.Time) error {
	dir := path.Dir(rel)
	if err := u.plain.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if info, err := u.plain.Lstat(rel); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("output %s exists and is not a regular file", rel)
	}
	tmp, err := u.stage(mtime, func(w io.Writer) error {
		_, err := w.Write(body)
		return err
	})
	if err != nil {
		return err
	}
	if err := u.plain.Rename(tmp, rel); err != nil {
		_ = u.plain.Remove(tmp)
		return err
	}
	u.syncDir(dir)
	return nil
}

func keyMatches(o object, m transforms.Manifest) error {
	if m.InstallID != o.installID {
		return fmt.Errorf("manifest install_id %q does not match key install=%s", m.InstallID, o.installID)
	}
	if m.SourceID != o.sourceID {
		return fmt.Errorf("manifest source_id %q does not match key source=%s", m.SourceID, o.sourceID)
	}
	return nil
}

func (u *unsealer) outputPath(o object, m transforms.Manifest) (string, error) {
	if err := keyMatches(o, m); err != nil {
		return "", err
	}
	native, err := nativeRelPath(m.NativePath)
	if err != nil {
		return "", err
	}
	// The shipper decompresses a .zst source before sealing it, so its payload is not zstd.
	if leaf := path.Base(native); len(leaf) > len(".zst") && strings.HasSuffix(leaf, ".zst") {
		native = strings.TrimSuffix(native, ".zst")
	}
	dir, err := u.installDir(o.installRel, o.installID)
	if err != nil {
		return "", err
	}
	// The ids are safe segments: the manifest schema constrains them and they match the key.
	return dir + "/" + m.SourceID + "/" + native, nil
}

// nativeRelPath turns a native path from any OS into a relative path under the source directory.
func nativeRelPath(native string) (string, error) {
	var out []string
	for _, seg := range strings.Split(strings.ReplaceAll(native, `\`, "/"), "/") {
		switch {
		case seg == "" || seg == ".":
			continue
		case seg == "..":
			return "", fmt.Errorf("native_path %q has a '..' segment", native)
		case strings.ContainsRune(seg, 0):
			return "", fmt.Errorf("native_path %q has a NUL byte", native)
		case len(out) == 0 && len(seg) == 2 && seg[1] == ':' && isASCIILetter(seg[0]):
			seg = seg[:1]
		}
		if windowsNames {
			if err := windowsName(seg); err != nil {
				return "", fmt.Errorf("native_path %q: %w", native, err)
			}
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("native_path %q has no path segment", native)
	}
	return strings.Join(out, "/"), nil
}

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// windowsNames is a variable so a test can check Windows' rules on any OS. A macOS or Linux path
// that Windows cannot hold fails its object there, named, rather than being renamed.
var windowsNames = runtime.GOOS == "windows"

// windowsReserved are the characters no Windows file name may hold, beside the path separators.
const windowsReserved = `<>:"|?*`

func windowsName(seg string) error {
	base, _, _ := strings.Cut(seg, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	device := base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" ||
		len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && '0' <= base[3] && base[3] <= '9'
	switch {
	case strings.ContainsAny(seg, windowsReserved) || strings.IndexFunc(seg, unicode.IsControl) >= 0:
		return fmt.Errorf("%q has a character Windows does not allow in a file name", seg)
	case strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " "):
		return fmt.Errorf("%q ends in a dot or space, which Windows drops", seg)
	case device:
		return fmt.Errorf("%q is a Windows device name", seg)
	}
	return nil
}

// installDir names an install's output directory once per run, warning once about a bad name.
// An unreadable tags.json fails the object instead, so no output lands under the wrong directory.
func (u *unsealer) installDir(installRel, installID string) (string, error) {
	if dir, ok := u.installDirs[installRel]; ok {
		return dir, nil
	}
	tagsPath := installRel + "/tags.json"
	raw, err := u.readTags(tagsPath)
	if err != nil {
		return "", fmt.Errorf("%s: %w", tagsPath, err)
	}
	dir := installID
	name, err := installName(raw)
	if err != nil {
		fmt.Fprintf(u.stderr, "warning %s: %s; using the install id\n", oneLine(tagsPath), oneLine(err.Error()))
	}
	if name != "" {
		dir = fmt.Sprintf("%s (%s)", name, installID[:min(8, len(installID))])
	}
	u.installDirs[installRel] = dir
	return dir, nil
}

// readTags is nil with no error when tags.json is absent or not a regular file, and reads at most one
// byte past maxTagsBytes.
func (u *unsealer) readTags(tagsPath string) ([]byte, error) {
	info, err := u.enc.Lstat(tagsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil
	}
	f, err := u.enc.Open(tagsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxTagsBytes+1))
}

// installName is "" with no error when there is no tags.json or it has no name.
func installName(raw []byte) (string, error) {
	if raw == nil {
		return "", nil
	}
	if len(raw) > maxTagsBytes {
		return "", fmt.Errorf("over %d bytes", maxTagsBytes)
	}
	var tags struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tags); err != nil {
		return "", err
	}
	if tags.Name == nil {
		return "", nil
	}
	name := strings.TrimSpace(*tags.Name)
	// The directory is "<name> (<8-char id>)", 11 bytes longer than the name; the suffix also keeps a
	// trailing dot or a device name from reaching Windows, which leaves its reserved characters.
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00"+windowsReserved) ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 || len(name)+11 > maxNameBytes {
		return "", fmt.Errorf("name %q is not a safe directory name", *tags.Name)
	}
	return name, nil
}

// upToDate compares the sidecar, which carries shipped_hash and payload_size, so equal bytes there
// and an output of payload_size mean the same payload, whatever the mtime.
func (u *unsealer) upToDate(out string, m transforms.Manifest) (bool, error) {
	p, err := u.plain.Stat(out)
	if err != nil || !p.Mode().IsRegular() || p.Size() != m.PayloadSize {
		return false, nil
	}
	want, err := sidecarJSON(m)
	if err != nil {
		return false, err
	}
	got, err := u.plain.ReadFile(out + sidecarSuffix)
	return err == nil && bytes.Equal(got, want), nil
}

// write stages payload and sidecar before renaming either, so a failed object leaves no new file.
func (u *unsealer) write(obj *io.SectionReader, out string, mtime time.Time) (err error) {
	dir := path.Dir(out)
	if err := u.plain.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, p := range []string{out, out + sidecarSuffix} {
		if info, err := u.plain.Lstat(p); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("output %s exists and is not a regular file", p)
		}
	}
	var payload, sidecar string
	defer func() {
		// Removing a temp name already renamed away is a harmless no-op.
		for _, tmp := range []string{payload, sidecar} {
			if err != nil && tmp != "" {
				_ = u.plain.Remove(tmp)
			}
		}
	}()
	var m transforms.Manifest
	if payload, err = u.stage(mtime, func(w io.Writer) (err error) {
		m, err = transforms.OpenTo(io.NewSectionReader(obj, 0, obj.Size()), w, u.ids...)
		return err
	}); err != nil {
		return err
	}
	if sidecar, err = u.stage(mtime, func(w io.Writer) error {
		b, err := sidecarJSON(m)
		if err == nil {
			_, err = w.Write(b)
		}
		return err
	}); err != nil {
		return err
	}
	// Sidecar first: its longer name fails before the payload is replaced.
	if err = u.plain.Rename(sidecar, out+sidecarSuffix); err != nil {
		return err
	}
	if err = u.plain.Rename(payload, out); err != nil {
		return err
	}
	u.syncDir(dir)
	return nil
}

func sidecarJSON(m transforms.Manifest) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(m)
	return b.Bytes(), err
}

// stage writes a synced file in stagingDir stamped with mtime, for a rename into place.
func (u *unsealer) stage(mtime time.Time, fill func(io.Writer) error) (string, error) {
	tmp := path.Join(stagingDir, tempName())
	f, err := u.plain.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	err = fill(f)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = u.plain.Chtimes(tmp, mtime, mtime)
	}
	if err != nil {
		_ = u.plain.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

func tempName() string {
	var name [8]byte
	_, _ = rand.Read(name[:])
	return hex.EncodeToString(name[:])
}

// syncDir makes the renames durable, best-effort like platform.syncDir: Windows cannot sync a directory.
func (u *unsealer) syncDir(dir string) {
	if d, err := u.plain.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// oneLine keeps a diagnostic on one terminal line even when it carries untrusted archive bytes.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", "; ")
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return strconv.Quote(s)
	}
	return s
}
