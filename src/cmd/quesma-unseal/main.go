// Command quesma-unseal decrypts a bucket archive copied locally by rclone into a readable tree,
// PLAIN_DIR/<install>/<source_id>/<native_path> plus a .manifest.json sidecar per object, or with
// -reseal-to writes each object resealed to one more recipient under its own key, for rclone to
// upload. The archive is written by developer machines and untrusted, so every write is confined by
// os.Root.
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
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"filippo.io/age"
	"golang.org/x/text/unicode/norm"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

const usage = `usage: quesma-unseal -i IDENTITY_FILE [-i IDENTITY_FILE]... ENC_DIR PLAIN_DIR
       quesma-unseal -i IDENTITY_FILE [-i IDENTITY_FILE]... -reseal-to RECIPIENT [-reseal-to RECIPIENT]... ENC_DIR OUT_DIR`

const sidecarSuffix = ".manifest.json"

// maxTagsBytes bounds tags.json, which holds a name and a few labels; a huge one is hostile.
const maxTagsBytes = 1 << 20

// maxNameBytes is NAME_MAX on APFS and ext4.
const maxNameBytes = 255

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	start := time.Now()
	flags := flag.NewFlagSet("quesma-unseal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var idFiles []string
	flags.Func("i", "age identity file of AGE-SECRET-KEY-1 lines, as age-keygen writes (repeatable)", func(v string) error {
		idFiles = append(idFiles, v)
		return nil
	})
	var resealTo []string
	flags.Func("reseal-to", "age recipient to add to every object not sealed to it (repeatable)", func(v string) error {
		r, err := age.ParseX25519Recipient(v)
		if err == nil {
			resealTo = append(resealTo, r.String())
		}
		return err
	})
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "quesma-unseal: %v\n%s\n", err, usage)
		return 2
	}
	if flags.NArg() != 2 || len(idFiles) == 0 {
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
	if len(resealTo) > 0 {
		outName, work, done, kept = "OUT_DIR", (*unsealer).reseal, "resealed", "already sealed"
	}
	plain, err := openPlainDir(flags.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: %s: %v\n", outName, err)
		return 2
	}
	defer plain.Close()

	u := &unsealer{
		enc: enc, plain: plain, ids: ids, resealTo: resealTo, stderr: stderr,
		installDirs: map[string]string{}, claimed: map[string]string{},
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
	claimed     map[string]string // case- and NFC-folded output path -> .age rel path that claimed it

	decrypted, unchanged, failed int // under -reseal-to: resealed, already sealed, failed
}

// object is one mirror .age file, with the ids its key claims for it.
type object struct {
	rel, installRel, installID, sourceID string
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
	for _, p := range []string{out, out + sidecarSuffix} {
		// Case- and NFC-folded, since macOS and Windows would silently merge the two files.
		key := strings.ToLower(norm.NFC.String(p))
		if prev, ok := u.claimed[key]; ok {
			return false, fmt.Errorf("output %s already written by %s", p, prev)
		}
		u.claimed[key] = o.rel
	}

	if ok, err := u.upToDate(out, m); err != nil || ok {
		return false, err
	}
	return true, u.write(obj, out, info.ModTime())
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
	tmp, err := u.stage(dir, mtime, func(w io.Writer) error {
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
		out = append(out, seg)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("native_path %q has no path segment", native)
	}
	return strings.Join(out, "/"), nil
}

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

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
	// The directory is "<name> (<8-char id>)", 11 bytes longer than the name.
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || len(name)+11 > maxNameBytes {
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
	if payload, err = u.stage(dir, mtime, func(w io.Writer) (err error) {
		m, err = transforms.OpenTo(io.NewSectionReader(obj, 0, obj.Size()), w, u.ids...)
		return err
	}); err != nil {
		return err
	}
	if sidecar, err = u.stage(dir, mtime, func(w io.Writer) error {
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

// stage writes a synced temp file in dir stamped with mtime; its fixed-length name fits beside any leaf.
func (u *unsealer) stage(dir string, mtime time.Time, fill func(io.Writer) error) (string, error) {
	tmp := path.Join(dir, tempName("tmp"))
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

func tempName(kind string) string {
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	return ".unseal-" + hex.EncodeToString(suffix[:]) + "." + kind
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
