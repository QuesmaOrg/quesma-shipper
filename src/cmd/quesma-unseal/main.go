// Command quesma-unseal decrypts a bucket archive copied locally by rclone into a readable tree,
// PLAIN_DIR/<install>/<source_id>/<native_path> plus a .manifest.json sidecar per object. The
// archive is written by developer machines and untrusted, so every write is confined by os.Root.
package main

import (
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

const usage = "usage: quesma-unseal -i IDENTITY_FILE [-i IDENTITY_FILE]... ENC_DIR PLAIN_DIR"

const sidecarSuffix = ".manifest.json"

// maxTagsBytes bounds tags.json, which holds a name and a few labels; a huge one is hostile.
const maxTagsBytes = 1 << 20

// maxNameBytes is NAME_MAX on APFS and ext4.
const maxNameBytes = 255

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type identityFiles []string

func (f *identityFiles) String() string { return strings.Join(*f, ",") }

func (f *identityFiles) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	start := time.Now()
	flags := flag.NewFlagSet("quesma-unseal", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var idFiles identityFiles
	flags.Var(&idFiles, "i", "age identity file, as for age -d -i (repeatable)")
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
	plain, err := openPlainDir(flags.Arg(1))
	if err != nil {
		fmt.Fprintf(stderr, "quesma-unseal: PLAIN_DIR: %v\n", err)
		return 2
	}
	defer plain.Close()

	u := &unsealer{
		enc: enc, plain: plain, ids: ids, stderr: stderr,
		tags: map[string]bool{}, installDirs: map[string]string{}, claimed: map[string]string{},
	}
	defer u.removeProbe()
	for _, o := range u.collect() {
		decrypted, err := u.object(o)
		switch {
		case err != nil:
			u.fail(o.rel, err)
		case decrypted:
			u.decrypted++
		default:
			u.unchanged++
		}
	}
	fmt.Fprintf(stdout, "decrypted %d, unchanged %d, failed %d in %.1fs\n",
		u.decrypted, u.unchanged, u.failed, time.Since(start).Seconds())
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
	stderr     io.Writer

	tags        map[string]bool   // install dir rel path -> has a regular tags.json
	installDirs map[string]string // install dir rel path -> output directory name
	claimed     map[string]string // case- and NFC-folded output path -> .age rel path that claimed it
	probe       string            // PLAIN_DIR file that shows how its filesystem stores an mtime

	decrypted, unchanged, failed int
}

// object is one mirror .age file, with the ids its key claims for it.
type object struct {
	rel, installRel, installID, sourceID string
}

func (u *unsealer) fail(rel string, err error) {
	u.failed++
	fmt.Fprintf(u.stderr, "unseal %s: %s\n", oneLine(rel), oneLine(err.Error()))
}

// collect walks ENC_DIR lexically, finding install=<id> at any depth, never following symlinks.
func (u *unsealer) collect() []object {
	var objects []object
	_ = fs.WalkDir(u.enc.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			u.fail(p, err)
			return nil
		}
		segs := strings.Split(p, "/")
		n := len(segs)
		if d.Type()&fs.ModeSymlink != 0 {
			switch {
			case archiveSlot(segs):
				u.fail(p, errors.New("symlink, not followed"))
			case !slices.ContainsFunc(segs, isInstall):
				fmt.Fprintf(u.stderr, "warning %s: symlink, not followed\n", oneLine(p))
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		switch {
		case n >= 2 && segs[n-1] == "tags.json" && isInstall(segs[n-2]):
			if d.Type().IsRegular() {
				u.tags[path.Dir(p)] = true
			}
		case isMirrorObject(segs):
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

func isInstall(seg string) bool { return strings.HasPrefix(seg, "install=") }

func isMirrorObject(segs []string) bool {
	n := len(segs)
	return n >= 4 && isInstall(segs[n-4]) && segs[n-3] == "mirror" &&
		strings.HasPrefix(segs[n-2], "source=") && strings.HasSuffix(segs[n-1], ".age")
}

// archiveSlot reports whether a symlink sits where an object, a tags.json or a directory of them would.
func archiveSlot(segs []string) bool {
	n := len(segs)
	at := func(i int) string {
		if i < 0 {
			return ""
		}
		return segs[i]
	}
	return isInstall(at(n-1)) ||
		isInstall(at(n-2)) && (at(n-1) == "mirror" || at(n-1) == "tags.json") ||
		isInstall(at(n-3)) && at(n-2) == "mirror" && strings.HasPrefix(at(n-1), "source=") ||
		isMirrorObject(segs)
}

// object brings one output up to date and reports whether it decrypted the payload.
func (u *unsealer) object(o object) (bool, error) {
	f, err := u.enc.Open(o.rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("not a regular file")
	}
	obj := io.NewSectionReader(f, 0, info.Size())
	m, err := u.manifest(obj)
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

	if u.upToDate(out, info.ModTime()) {
		return false, nil
	}
	return true, u.write(obj, out, info.ModTime())
}

// manifest reads the manifest from a prefix of the object, doubling it as ErrPrefixTooShort asks.
func (u *unsealer) manifest(obj *io.SectionReader) (transforms.Manifest, error) {
	for n := int64(transforms.SuggestedPrefixBytes); ; n = min(2*n, transforms.MaxPrefixBytes) {
		prefix, err := io.ReadAll(io.NewSectionReader(obj, 0, n))
		if err != nil {
			return transforms.Manifest{}, err
		}
		m, err := transforms.ReadManifestPrefix(prefix, u.ids...)
		if err == nil {
			return m, nil
		}
		if int64(len(prefix)) < n || n == transforms.MaxPrefixBytes {
			break
		}
	}
	// ReadManifestPrefix reports every failure as too short; OpenTo surfaces the real cause.
	if _, err := transforms.OpenTo(io.NewSectionReader(obj, 0, obj.Size()), io.Discard, u.ids...); err != nil {
		return transforms.Manifest{}, err
	}
	return transforms.Manifest{}, fmt.Errorf("manifest not within the first %d bytes", transforms.MaxPrefixBytes)
}

func (u *unsealer) outputPath(o object, m transforms.Manifest) (string, error) {
	if m.InstallID != o.installID {
		return "", fmt.Errorf("manifest install_id %q does not match key install=%s", m.InstallID, o.installID)
	}
	if m.SourceID != o.sourceID {
		return "", fmt.Errorf("manifest source_id %q does not match key source=%s", m.SourceID, o.sourceID)
	}
	native, err := nativeRelPath(m.NativePath)
	if err != nil {
		return "", err
	}
	if leaf := path.Base(native) + sidecarSuffix; len(leaf) > maxNameBytes {
		return "", fmt.Errorf("output name %s is over %d bytes", leaf, maxNameBytes)
	}
	return u.installDir(o.installRel, o.installID) + "/" + m.SourceID + "/" + native, nil
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
func (u *unsealer) installDir(installRel, installID string) string {
	if dir, ok := u.installDirs[installRel]; ok {
		return dir
	}
	dir := installID
	name, warning := u.installName(installRel)
	if warning != "" {
		fmt.Fprintf(u.stderr, "warning %s/tags.json: %s; using the install id\n", oneLine(installRel), oneLine(warning))
	}
	if name != "" {
		dir = fmt.Sprintf("%s (%s)", name, installID[:min(8, len(installID))])
	}
	u.installDirs[installRel] = dir
	return dir
}

func (u *unsealer) installName(installRel string) (name, warning string) {
	if !u.tags[installRel] {
		return "", ""
	}
	f, err := u.enc.Open(installRel + "/tags.json")
	if err != nil {
		return "", err.Error()
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxTagsBytes+1))
	if err != nil {
		return "", err.Error()
	}
	if len(raw) > maxTagsBytes {
		return "", fmt.Sprintf("over %d bytes", maxTagsBytes)
	}
	var tags struct {
		Name *string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tags); err != nil {
		return "", err.Error()
	}
	if tags.Name == nil {
		return "", ""
	}
	name = strings.TrimSpace(*tags.Name)
	// The directory is "<name> (<8-char id>)", 11 bytes longer than the name.
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || len(name)+11 > maxNameBytes {
		return "", fmt.Sprintf("name %q is not a safe directory name", *tags.Name)
	}
	return name, ""
}

func (u *unsealer) upToDate(out string, mtime time.Time) bool {
	p, err := u.plain.Stat(out)
	if err != nil || !p.Mode().IsRegular() || !p.ModTime().Equal(mtime) && !p.ModTime().Equal(u.stored(mtime)) {
		return false
	}
	s, err := u.plain.Stat(out + sidecarSuffix)
	return err == nil && s.Mode().IsRegular()
}

// stored is mtime as PLAIN_DIR keeps it, since FAT, HFS+ and some mounts round what Chtimes sets.
func (u *unsealer) stored(mtime time.Time) time.Time {
	if u.probe == "" {
		name := tempName("probe")
		f, err := u.plain.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return mtime
		}
		f.Close()
		u.probe = name
	}
	if err := setMtime(u.plain, u.probe, mtime); err != nil {
		return mtime
	}
	info, err := u.plain.Stat(u.probe)
	if err != nil {
		return mtime
	}
	return info.ModTime()
}

func (u *unsealer) removeProbe() {
	if u.probe != "" {
		_ = u.plain.Remove(u.probe)
	}
}

// setMtime is a variable so a test can play a filesystem with coarse timestamps.
var setMtime = func(root *os.Root, name string, mtime time.Time) error {
	return root.Chtimes(name, mtime, mtime)
}

// write stages payload and sidecar before renaming either, so a failed object leaves no new file.
func (u *unsealer) write(obj *io.SectionReader, out string, mtime time.Time) error {
	dir := path.Dir(out)
	if err := u.plain.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, p := range []string{out, out + sidecarSuffix} {
		if info, err := u.plain.Lstat(p); err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("output %s exists and is not a regular file", p)
		}
	}
	var m transforms.Manifest
	payload, err := u.stage(dir, mtime, func(w io.Writer) (err error) {
		m, err = transforms.OpenTo(io.NewSectionReader(obj, 0, obj.Size()), w, u.ids...)
		return err
	})
	if err != nil {
		return err
	}
	sidecar, err := u.stage(dir, mtime, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(m)
	})
	if err != nil {
		_ = u.plain.Remove(payload)
		return err
	}
	// Sidecar first: the payload's mtime is what marks the pair complete for the next run.
	if err := u.plain.Rename(sidecar, out+sidecarSuffix); err != nil {
		_ = u.plain.Remove(sidecar)
		_ = u.plain.Remove(payload)
		return err
	}
	if err := u.plain.Rename(payload, out); err != nil {
		_ = u.plain.Remove(payload)
		return err
	}
	u.syncDir(dir)
	return nil
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
		err = setMtime(u.plain, tmp, mtime)
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
