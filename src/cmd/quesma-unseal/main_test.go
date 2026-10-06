package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

const (
	org      = "v1/organization=acme"
	installA = "3f2a1b2c-0000-4000-8000-000000000001"
	installB = "9b8c7d6e-0000-4000-8000-000000000002"
	claude   = "claude-code-transcripts"
	cursor   = "cursor-transcripts"
)

var t0 = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

type archive struct {
	t                 *testing.T
	enc, plain, key   string
	id                *age.X25519Identity
	stdout, stderrStr string
}

func newArchive(t *testing.T) *archive {
	t.Helper()
	dir := t.TempDir()
	a := &archive{t: t, enc: filepath.Join(dir, "enc"), plain: filepath.Join(dir, "plain")}
	a.id, a.key = newKey(t, dir, "custodian.agekey")
	return a
}

func newKey(t *testing.T, dir, name string) (*age.X25519Identity, string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("# test custodian\n"+id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return id, p
}

func manifest(install, source, native string) transforms.Manifest {
	return transforms.Manifest{
		ManifestVersion: transforms.ManifestVersion,
		OrganizationID:  "acme",
		InstallID:       install,
		SourceID:        source,
		NativePath:      native,
		Gather:          "file_glob",
		ArtifactClass:   "trajectory",
		SourceHash:      strings.Repeat("a", 64),
		SealedAt:        "2026-07-30T10:00:00Z",
		Client:          transforms.Client{Version: "0.1.0"},
	}
}

// installRoot is the producer's own key prefix, so the tests follow any layout change.
func installRoot(install string) string {
	root, err := formats.InstallRoot("acme", install)
	if err != nil {
		panic(err)
	}
	return root
}

func objectKey(install, source, leaf string) string {
	return fmt.Sprintf("%s/mirror/source=%s/%s.age", installRoot(install), source, leaf)
}

// out is an output path under PLAIN_DIR.
func out(installDir, source, rel string) string { return installDir + "/" + source + "/" + rel }

// put seals payload into ENC_DIR/key with the given mtime, to the custodian unless told otherwise.
func (a *archive) put(key string, m transforms.Manifest, payload string, mtime time.Time, to ...age.Recipient) {
	a.t.Helper()
	if len(to) == 0 {
		to = []age.Recipient{a.id.Recipient()}
	}
	obj, _, err := transforms.Seal(m, transforms.Unscrubbed([]byte(payload), "test fixture"), to)
	if err != nil {
		a.t.Fatal(err)
	}
	a.write(key, obj, mtime)
}

func (a *archive) write(key string, body []byte, mtime time.Time) {
	a.t.Helper()
	p := filepath.Join(a.enc, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		a.t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		a.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		a.t.Fatal(err)
	}
}

func (a *archive) tags(install, name string) {
	a.t.Helper()
	body, err := json.Marshal(map[string]any{
		"schema": 1, "install_id": install, "name": name,
		"metadata": map[string]string{"team": "x"}, "updated_at": "2026-07-01T00:00:00Z",
	})
	if err != nil {
		a.t.Fatal(err)
	}
	a.write(installRoot(install)+"/tags.json", body, t0)
}

// run invokes the command in process with the custodian key plus args before the two dirs.
func (a *archive) run(args ...string) int {
	a.t.Helper()
	return a.runIn(a.enc, args...)
}

func (a *archive) runIn(enc string, args ...string) int {
	a.t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append(append([]string{"-i", a.key}, args...), enc, a.plain), &stdout, &stderr)
	a.stdout, a.stderrStr = stdout.String(), stderr.String()
	return code
}

func (a *archive) wantSummary(code, wantCode, decrypted, unchanged, failed int) {
	a.t.Helper()
	re := regexp.MustCompile(fmt.Sprintf(`^decrypted %d, unchanged %d, failed %d in \d+\.\ds\n$`, decrypted, unchanged, failed))
	if code != wantCode || !re.MatchString(a.stdout) {
		a.t.Fatalf("exit %d (want %d), stdout %q, stderr:\n%s", code, wantCode, a.stdout, a.stderrStr)
	}
}

func (a *archive) wantStderr(lines ...string) {
	a.t.Helper()
	want := ""
	if len(lines) > 0 {
		want = strings.Join(lines, "\n") + "\n"
	}
	if a.stderrStr != want {
		a.t.Fatalf("stderr:\n%s\nwant:\n%s", a.stderrStr, want)
	}
}

// readPlain returns a PLAIN_DIR file's bytes after checking its mtime.
func (a *archive) readPlain(rel string, mtime time.Time) []byte {
	a.t.Helper()
	p := filepath.Join(a.plain, filepath.FromSlash(rel))
	got, err := os.ReadFile(p)
	if err != nil {
		a.t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		a.t.Fatal(err)
	}
	if !info.ModTime().Equal(mtime) {
		a.t.Fatalf("%s mtime %v, want %v", rel, info.ModTime(), mtime)
	}
	return got
}

func (a *archive) wantFile(rel, body string, mtime time.Time) {
	a.t.Helper()
	if got := a.readPlain(rel, mtime); string(got) != body {
		a.t.Fatalf("%s = %q, want %q", rel, got, body)
	}
}

func (a *archive) wantSidecar(rel, native string, mtime time.Time) {
	a.t.Helper()
	raw := a.readPlain(rel+sidecarSuffix, mtime)
	var m transforms.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		a.t.Fatal(err)
	}
	if m.NativePath != native || m.ShippedHash == "" {
		a.t.Fatalf("sidecar %s: native_path %q shipped_hash %q", rel, m.NativePath, m.ShippedHash)
	}
	// Readable as written: "&" stays literal rather than \u0026.
	if want := `"native_path": "` + strings.ReplaceAll(native, `\`, `\\`) + `"`; !strings.Contains(string(raw), want) {
		a.t.Fatalf("sidecar %s lacks %s:\n%s", rel, want, raw)
	}
}

// listPlain returns every file under PLAIN_DIR, slash-separated, for exact-layout assertions.
func (a *archive) listPlain() []string {
	a.t.Helper()
	var files []string
	err := filepath.WalkDir(a.plain, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(a.plain, p)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		a.t.Fatal(err)
	}
	return files
}

// wantPlain checks PLAIN_DIR holds exactly these payloads, each with its sidecar.
func (a *archive) wantPlain(payloads ...string) {
	a.t.Helper()
	var files []string
	for _, p := range payloads {
		files = append(files, p, p+sidecarSuffix)
	}
	sort.Strings(files)
	if got := a.listPlain(); strings.Join(got, "\n") != strings.Join(files, "\n") {
		a.t.Fatalf("plain tree:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(files, "\n"))
	}
}

func TestLayout(t *testing.T) {
	a := newArchive(t)
	a.tags(installA, "  Jacek MBP ")
	claudeNative := "/Users/__USER__/.claude/projects/-Users-__USER__-R&D/1111.jsonl"
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, claudeNative), "claude\n", t0)
	a.put(objectKey(installA, cursor, "22bb"), manifest(installA, cursor,
		`C:\Users\__USER__\.cursor\chats\2222.jsonl`), "cursor\n", t0.Add(time.Hour))
	a.put(objectKey(installB, claude, "33cc"), manifest(installB, claude,
		"/Users/__USER__/.claude/projects/proj/a.jsonl"), "unnamed\n", t0)
	a.write(installRoot(installB)+"/state/cursor.json", []byte("{}"), t0)
	a.write(installRoot(installB)+"/heartbeat.json", []byte("{}"), t0)

	claudeOut := out("Jacek MBP (3f2a1b2c)", claude, "Users/__USER__/.claude/projects/-Users-__USER__-R&D/1111.jsonl")
	cursorOut := out("Jacek MBP (3f2a1b2c)", cursor, "C/Users/__USER__/.cursor/chats/2222.jsonl")
	unnamedOut := out(installB, claude, "Users/__USER__/.claude/projects/proj/a.jsonl")
	a.wantSummary(a.run(), 0, 3, 0, 0)
	a.wantStderr()
	a.wantPlain(claudeOut, cursorOut, unnamedOut)
	a.wantFile(claudeOut, "claude\n", t0)
	a.wantSidecar(claudeOut, claudeNative, t0)
	a.wantFile(cursorOut, "cursor\n", t0.Add(time.Hour))
	a.wantFile(unnamedOut, "unnamed\n", t0)
	a.wantSidecar(cursorOut, `C:\Users\__USER__\.cursor\chats\2222.jsonl`, t0.Add(time.Hour))
}

func TestSecondRunUnchanged(t *testing.T) {
	a := newArchive(t)
	a.tags(installA, "laptop")
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/1.jsonl"), "one\n", t0)
	a.put(objectKey(installB, claude, "22bb"), manifest(installB, claude, "/a/2.jsonl"), "two\n", t0)

	// The organization root is as good an ENC_DIR as the bucket root.
	orgRoot := filepath.Join(a.enc, filepath.FromSlash(org))
	a.wantSummary(a.runIn(orgRoot), 0, 2, 0, 0)
	a.wantSummary(a.runIn(orgRoot), 0, 0, 2, 0)
	a.wantSummary(a.run(), 0, 0, 2, 0)
	a.wantStderr()
}

func TestResealedObjectRewrites(t *testing.T) {
	a := newArchive(t)
	key := objectKey(installA, claude, "11aa")
	a.put(key, manifest(installA, claude, "/a/1.jsonl"), "v1\n", t0)
	a.put(objectKey(installA, claude, "22bb"), manifest(installA, claude, "/a/2.jsonl"), "other\n", t0)
	a.wantSummary(a.run(), 0, 2, 0, 0)

	a.put(key, manifest(installA, claude, "/a/1.jsonl"), "v1\nv2\n", t0.Add(time.Minute))
	a.wantSummary(a.run(), 0, 1, 1, 0)
	a.wantFile(out(installA, claude, "a/1.jsonl"), "v1\nv2\n", t0.Add(time.Minute))

	// A sidecar missing alone also forces a rewrite.
	if err := os.Remove(filepath.Join(a.plain, installA, claude, "a", "2.jsonl"+sidecarSuffix)); err != nil {
		t.Fatal(err)
	}
	a.wantSummary(a.run(), 0, 1, 1, 0)
	a.wantSidecar(out(installA, claude, "a/2.jsonl"), "/a/2.jsonl", t0)
}

func TestInstallRenameKeepsOldDir(t *testing.T) {
	a := newArchive(t)
	a.tags(installA, "old name")
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/1.jsonl"), "x\n", t0)
	a.wantSummary(a.run(), 0, 1, 0, 0)

	a.tags(installA, "new name")
	a.wantSummary(a.run(), 0, 1, 0, 0)
	a.wantPlain(out("new name (3f2a1b2c)", claude, "a/1.jsonl"), out("old name (3f2a1b2c)", claude, "a/1.jsonl"))
}

func TestWrongIdentityFailsOnlyThatObject(t *testing.T) {
	a := newArchive(t)
	other, otherKey := newKey(t, t.TempDir(), "other.agekey")
	bad := objectKey(installA, cursor, "77ac")
	a.put(bad, manifest(installA, cursor, "/a/bad.jsonl"), "secret\n", t0, other.Recipient())
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/good.jsonl"), "good\n", t0)

	a.wantSummary(a.run(), 1, 1, 0, 1)
	a.wantStderr("unseal " + bad + ": age decrypt: identity did not match any of the recipients: incorrect identity for recipient block")
	a.wantPlain(out(installA, claude, "a/good.jsonl"))

	a.wantSummary(a.run(), 1, 0, 1, 1)

	// -i repeats, like age -d -i; with the right key the failed object is retried and lands.
	a.wantSummary(a.run("-i", otherKey), 0, 1, 1, 0)
	a.wantFile(out(installA, cursor, "a/bad.jsonl"), "secret\n", t0)
}

func TestTraversalRejected(t *testing.T) {
	a := newArchive(t)
	cases := map[string]string{
		"01": "../../escape.jsonl",
		"02": `/Users/__USER__/..\..\..\escape.jsonl`,
		"03": "///",
	}
	for leaf, native := range cases {
		a.put(objectKey(installA, claude, leaf), manifest(installA, claude, native), "x\n", t0)
	}
	a.wantSummary(a.run(), 1, 0, 0, 3)
	a.wantStderr(
		"unseal "+objectKey(installA, claude, "01")+`: native_path "../../escape.jsonl" has a '..' segment`,
		"unseal "+objectKey(installA, claude, "02")+`: native_path "/Users/__USER__/..\\..\\..\\escape.jsonl" has a '..' segment`,
		"unseal "+objectKey(installA, claude, "03")+`: native_path "///" has no path segment`,
	)
	a.wantPlain()
	if entries, _ := os.ReadDir(filepath.Dir(a.plain)); len(entries) != 3 { // enc, plain, custodian.agekey
		t.Fatalf("something escaped PLAIN_DIR: %v", entries)
	}
}

func TestManifestKeyMismatchRejected(t *testing.T) {
	a := newArchive(t)
	a.put(objectKey(installA, claude, "11aa"), manifest(installB, claude, "/a/1.jsonl"), "x\n", t0)
	a.put(objectKey(installA, claude, "22bb"), manifest(installA, cursor, "/a/2.jsonl"), "x\n", t0)
	a.wantSummary(a.run(), 1, 0, 0, 2)
	a.wantStderr(
		"unseal "+objectKey(installA, claude, "11aa")+`: manifest install_id "`+installB+`" does not match key install=`+installA,
		"unseal "+objectKey(installA, claude, "22bb")+`: manifest source_id "cursor-transcripts" does not match key source=claude-code-transcripts`,
	)
	a.wantPlain()
}

func TestUnsafeInstallNameWarnsOnce(t *testing.T) {
	for _, name := range []string{"../evil", `a\b`, "..", " . ", "   ", "nul\x00byte", strings.Repeat("名", 82)} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			a := newArchive(t)
			a.tags(installA, name)
			a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/1.jsonl"), "x\n", t0)
			a.put(objectKey(installA, claude, "22bb"), manifest(installA, claude, "/a/2.jsonl"), "y\n", t0)
			a.wantSummary(a.run(), 0, 2, 0, 0)
			a.wantStderr(fmt.Sprintf("warning %s/install=%s/tags.json: name %q is not a safe directory name; using the install id",
				org, installA, name))
			a.wantFile(out(installA, claude, "a/2.jsonl"), "y\n", t0)
		})
	}
}

func TestSymlinksNotFollowed(t *testing.T) {
	a := newArchive(t)
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/1.jsonl"), "x\n", t0)

	// A real object outside ENC_DIR, reachable only through symlinks.
	outside := &archive{t: t, enc: filepath.Join(t.TempDir(), "enc"), id: a.id}
	outside.put(objectKey(installB, claude, "22bb"), manifest(installB, claude, "/a/2.jsonl"), "outside\n", t0)
	srcDir := filepath.Join(outside.enc, filepath.FromSlash(org), "install="+installB, "mirror", "source="+claude)
	linkDir := filepath.Join(a.enc, filepath.FromSlash(org), "install="+installB, "mirror", "source="+claude)
	if err := os.MkdirAll(filepath.Dir(linkDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(srcDir, linkDir); err != nil {
		t.Fatal(err)
	}
	linkFile := filepath.Join(a.enc, filepath.FromSlash(objectKey(installA, claude, "33cc")))
	if err := os.Symlink(filepath.Join(srcDir, "22bb.age"), linkFile); err != nil {
		t.Fatal(err)
	}

	a.wantSummary(a.run(), 1, 1, 0, 2)
	a.wantStderr(
		"unseal "+objectKey(installA, claude, "33cc")+": symlink, not followed",
		"unseal "+installRoot(installB)+"/mirror/source="+claude+": symlink, not followed",
	)
	a.wantPlain(out(installA, claude, "a/1.jsonl"))
}

// Case and NFC/NFD spellings collide too, since macOS and Windows merge them into one file.
func TestDuplicateOutputPath(t *testing.T) {
	a := newArchive(t)
	nfc, nfd := "a/café.jsonl", "a/café.jsonl"
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/1.jsonl"), "first\n", t0)
	a.put(objectKey(installA, claude, "22bb"), manifest(installA, claude, `\a\1.jsonl`), "second\n", t0)
	a.put(objectKey(installA, claude, "33cc"), manifest(installA, claude, "/A/1.JSONL"), "third\n", t0)
	a.put(objectKey(installA, claude, "44dd"), manifest(installA, claude, "/"+nfc), "nfc\n", t0)
	a.put(objectKey(installA, claude, "55ee"), manifest(installA, claude, "/"+nfd), "nfd\n", t0)
	collides := func(leaf, output, first string) string {
		return "unseal " + objectKey(installA, claude, leaf) + ": output " + out(installA, claude, output) +
			" already written by " + objectKey(installA, claude, first)
	}
	want := []string{
		collides("22bb", "a/1.jsonl", "11aa"),
		collides("33cc", "A/1.JSONL", "11aa"),
		collides("55ee", nfd, "44dd"),
	}
	a.wantSummary(a.run(), 1, 2, 0, 3)
	a.wantStderr(want...)
	a.wantFile(out(installA, claude, "a/1.jsonl"), "first\n", t0)
	a.wantFile(out(installA, claude, nfc), "nfc\n", t0)

	a.wantSummary(a.run(), 1, 0, 2, 3)
	a.wantStderr(want...)
}

func TestUsageErrors(t *testing.T) {
	dir := t.TempDir()
	_, key := newKey(t, dir, "k.agekey")
	notKey := filepath.Join(dir, "not.agekey")
	if err := os.WriteFile(notKey, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no args":         nil,
		"no identity":     {dir, filepath.Join(dir, "plain")},
		"one dir":         {"-i", key, dir},
		"unknown flag":    {"-x", "-i", key, dir, filepath.Join(dir, "plain")},
		"missing key":     {"-i", filepath.Join(dir, "nope"), dir, filepath.Join(dir, "plain")},
		"not a key":       {"-i", notKey, dir, filepath.Join(dir, "plain")},
		"missing ENC_DIR": {"-i", key, filepath.Join(dir, "nope"), filepath.Join(dir, "plain")},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 || stdout.Len() != 0 || stderr.Len() == 0 {
			t.Errorf("%s: exit %d, stdout %q, stderr %q", name, code, stdout.String(), stderr.String())
		}
	}
}

func TestNativeRelPath(t *testing.T) {
	for native, want := range map[string]string{
		"/Users/__USER__/./x//y.jsonl": "Users/__USER__/x/y.jsonl",
		`C:\Users\__USER__\x.jsonl`:    "C/Users/__USER__/x.jsonl",
		`\\server\share\x.jsonl`:       "server/share/x.jsonl",
		"rel/C:/x":                     "rel/C:/x",
	} {
		if got, err := nativeRelPath(native); err != nil || got != want {
			t.Errorf("nativeRelPath(%q) = %q, %v; want %q", native, got, err, want)
		}
	}
	for _, native := range []string{"", "/", "a/../b", `a\..\b`, "a/nul\x00/b"} {
		if got, err := nativeRelPath(native); err == nil {
			t.Errorf("nativeRelPath(%q) = %q, want an error", native, got)
		}
	}
}

// A manifest past the first SuggestedPrefixBytes still leaves an unchanged object unchanged.
func TestLargeManifestUnchanged(t *testing.T) {
	a := newArchive(t)
	m := manifest(installA, claude, "/a/1.jsonl")
	rng := mathrand.New(mathrand.NewSource(1))
	for range 12000 {
		m.DerivedFrom = append(m.DerivedFrom, fmt.Sprintf("%016x%016x%016x%016x", rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Uint64()))
	}
	key := objectKey(installA, claude, "11aa")
	a.put(key, m, "x\n", t0)
	obj, err := os.ReadFile(filepath.Join(a.enc, filepath.FromSlash(key)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transforms.ReadManifestPrefix(obj[:transforms.SuggestedPrefixBytes], a.id); err == nil {
		t.Fatal("fixture manifest fits the suggested prefix")
	}

	a.wantSummary(a.run(), 0, 1, 0, 0)
	a.wantSummary(a.run(), 0, 0, 1, 0)
	a.wantFile(out(installA, claude, "a/1.jsonl"), "x\n", t0)
}

// PLAIN_DIR on FAT, HFS+ or a bind mount rounds the mtime it stores; that is still unchanged.
func TestCoarseMtimeFilesystem(t *testing.T) {
	orig := setMtime
	t.Cleanup(func() { setMtime = orig })
	setMtime = func(root *os.Root, name string, mtime time.Time) error {
		mtime = mtime.Truncate(time.Second)
		return root.Chtimes(name, mtime, mtime)
	}

	a := newArchive(t)
	key := objectKey(installA, claude, "11aa")
	a.put(key, manifest(installA, claude, "/a/1.jsonl"), "v1\n", t0.Add(500*time.Millisecond))
	a.wantSummary(a.run(), 0, 1, 0, 0)
	a.wantFile(out(installA, claude, "a/1.jsonl"), "v1\n", t0)
	a.wantSummary(a.run(), 0, 0, 1, 0)

	a.put(key, manifest(installA, claude, "/a/1.jsonl"), "v2\n", t0.Add(1500*time.Millisecond))
	a.wantSummary(a.run(), 0, 1, 0, 0)
	a.wantFile(out(installA, claude, "a/1.jsonl"), "v2\n", t0.Add(time.Second))
	a.wantPlain(out(installA, claude, "a/1.jsonl"))
}

// Symlinks that cannot hide an object, a tags.json or a directory of them are not failures.
func TestSymlinkOutsideArchiveIgnored(t *testing.T) {
	a := newArchive(t)
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/1.jsonl"), "x\n", t0)
	a.write(installRoot(installA)+"/state/cursor.json", []byte("{}"), t0)
	stateLink := filepath.Join(a.enc, filepath.FromSlash(org), "install="+installA, "state", "link.json")
	if err := os.Symlink(filepath.Join(filepath.Dir(stateLink), "cursor.json"), stateLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(a.enc, filepath.FromSlash(org)), filepath.Join(a.enc, "latest")); err != nil {
		t.Fatal(err)
	}

	a.wantSummary(a.run(), 0, 1, 0, 0)
	a.wantStderr("warning latest: symlink, not followed")
}

// A payload is streamed to disk, never held whole in memory, so a zstd bomb cannot exhaust it.
func TestPayloadStreamed(t *testing.T) {
	a := newArchive(t)
	const size = 128 << 20
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/zeros"), string(make([]byte, size)), t0)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	a.wantSummary(a.run(), 0, 1, 0, 0)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > size/4 {
		t.Fatalf("unsealing a %d-byte payload allocated %d bytes", size, alloc)
	}
	if info, err := os.Stat(filepath.Join(a.plain, installA, claude, "a", "zeros")); err != nil || info.Size() != size {
		t.Fatalf("payload: %v, %v", info, err)
	}
}

// A leaf as long as its sidecar name allows still unseals; a longer one fails and leaves nothing.
func TestLongLeaf(t *testing.T) {
	a := newArchive(t)
	fits := strings.Repeat("f", maxNameBytes-len(sidecarSuffix)-len(".jsonl")) + ".jsonl"
	tooLong := strings.Repeat("t", maxNameBytes-len(sidecarSuffix)-len(".jsonl")+1) + ".jsonl"
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/a/"+fits), "x\n", t0)
	a.put(objectKey(installA, claude, "22bb"), manifest(installA, claude, "/a/"+tooLong), "y\n", t0)

	a.wantSummary(a.run(), 1, 1, 0, 1)
	if !strings.HasPrefix(a.stderrStr, "unseal "+objectKey(installA, claude, "22bb")+": ") || strings.Count(a.stderrStr, "\n") != 1 {
		t.Fatalf("stderr:\n%s", a.stderrStr)
	}
	a.wantPlain(out(installA, claude, "a/"+fits))
}

// An output path that is already a directory fails before the sidecar is written.
func TestPayloadTargetIsDirectory(t *testing.T) {
	a := newArchive(t)
	a.put(objectKey(installA, claude, "11aa"), manifest(installA, claude, "/x/b/c"), "c\n", t0)
	a.put(objectKey(installA, claude, "22bb"), manifest(installA, claude, "/x/b"), "b\n", t0)

	a.wantSummary(a.run(), 1, 1, 0, 1)
	a.wantStderr("unseal " + objectKey(installA, claude, "22bb") + ": output " + installA +
		"/claude-code-transcripts/x/b exists and is not a regular file")
	a.wantPlain(out(installA, claude, "x/b/c"))
}
