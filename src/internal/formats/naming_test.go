package formats_test

import (
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

func keyA(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, formats.NameKeySize)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func keyB(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, formats.NameKeySize)
	for i := range k {
		k[i] = 0xff
	}
	return k
}

// Blinding is keyed, not a bare hash: a plaintext path hash in a listable key is an oracle.
func TestBlindingIsKeyed(t *testing.T) {
	const p = "projects/proj/a.jsonl"
	if formats.MirrorName(keyA(t), p) == formats.MirrorName(keyB(t), p) {
		t.Fatal("two name_keys produced the same mirror name")
	}
}

// A pathologically long native path leaves the key length unchanged.
func TestNameLengthIsFixed(t *testing.T) {
	long := strings.Repeat("verylongsegmentname/", 500) + "leaf.jsonl"
	short := formats.MirrorName(keyA(t), "a")
	huge := formats.MirrorName(keyA(t), formats.CanonicalPath(long, "jane"))

	if len(short) != 64 || len(huge) != 64 {
		t.Fatalf("mirror names must be 64 hex chars, got %d and %d", len(short), len(huge))
	}
}

// A file's whole history lands on one key, so a re-ship after state loss is an overwrite.
func TestNameIsStableAcrossCalls(t *testing.T) {
	p := formats.CanonicalPath("projects/-Users-jane-work-api/3f2504e0.jsonl", "jane")
	first := formats.MirrorName(keyA(t), p)
	for range 100 {
		if got := formats.MirrorName(keyA(t), p); got != first {
			t.Fatalf("mirror name is not deterministic: %s != %s", got, first)
		}
	}
}

// Home paths differing only by user name converge, so a renamed account does not re-ship.
func TestUserPlaceholderMakesPathsConverge(t *testing.T) {
	jane := formats.CanonicalPath("projects/-Users-jane-work-api/s.jsonl", "jane")
	bob := formats.CanonicalPath("projects/-Users-bob-work-api/s.jsonl", "bob")
	if jane != bob {
		t.Errorf("canonical paths should converge:\n jane %q\n bob  %q", jane, bob)
	}
}

// Over-replacing is the worse failure: it merges two different files onto one key.
func TestUserPlaceholderDoesNotOverMatch(t *testing.T) {
	cases := []struct{ in, username, want string }{
		{"notes/janes-notes.md", "jane", "notes/janes-notes.md"},
		{"notes/mariajane.txt", "jane", "notes/mariajane.txt"},
		{"jane/x.txt", "jane", "__USER__/x.txt"},
		{"-Users-jane-work", "jane", "-Users-__USER__-work"},
		{"a/b.txt", "a", "a/b.txt"},
		{"projects/x.jsonl", "", "projects/x.jsonl"},
	}
	for _, c := range cases {
		if got := formats.ApplyUserPlaceholder(c.in, c.username); got != c.want {
			t.Errorf("ApplyUserPlaceholder(%q, %q) = %q, want %q", c.in, c.username, got, c.want)
		}
	}
}

// The encoding has to be injective, or two files collide and one overwrites the other.
func TestSegmentEncodingIsInjective(t *testing.T) {
	seen := map[string]string{}
	for _, in := range []string{
		"a b", "a%20b", "a%2520b", "a/b", "a-b", "a_b", "a.b", "a~b",
		"100%", "100%25", "été", "%C3%A9",
	} {
		got := formats.CanonicalPath(in, "jane")
		if prev, dup := seen[got]; dup {
			t.Errorf("collision: %q and %q both encode to %q", prev, in, got)
		}
		seen[got] = in
	}
}

func TestMirrorKeyLayout(t *testing.T) {
	got, err := formats.MirrorKey("default", "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"claude-code-transcripts", keyA(t), "projects/proj/a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	want := "v1/organization=default/install=3f2504e0-4f89-41d3-9a0c-0305e82c3301/" +
		"mirror/source=claude-code-transcripts/" +
		"28547a64a206a3b4d14d27ccbf75019e758fc176524ac0ea7b0a83ef7df78ef4.age"
	if got != want {
		t.Errorf("key layout:\n got %s\nwant %s", got, want)
	}
}

// organization=default keeps every deployment's keys at one depth, so erasure is one sweep.
func TestStandaloneAndOrgKeysShareDepth(t *testing.T) {
	standalone, err := formats.MirrorKey("default", "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "s", keyA(t), "a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	enterprise, err := formats.MirrorKey("acme", "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "s", keyA(t), "a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := strings.Count(standalone, "/"), strings.Count(enterprise, "/"); a != b {
		t.Errorf("key depth differs: standalone %d segments, enterprise %d", a, b)
	}
}

// A source or org id carrying "/" or "=" could forge a path into another install's subtree.
func TestKeySegmentsAreValidated(t *testing.T) {
	const install = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	bad := []struct{ org, id, source string }{
		{"../../etc", install, "s"},
		{"default", install, "../other"},
		{"default", install, "a/b"},
		{"default", install, "install=other"},
		{"default", "", "s"},
		{"", install, "s"},
		{"default", install, "Capitals"},
	}
	for _, c := range bad {
		if _, err := formats.MirrorKey(c.org, c.id, c.source, keyA(t), "a.jsonl"); err == nil {
			t.Errorf("MirrorKey(%q, %q, %q) should have been refused", c.org, c.id, c.source)
		}
	}
}

func TestMirrorKeyRejectsWrongKeySize(t *testing.T) {
	if _, err := formats.MirrorKey("default", "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "s",
		[]byte("too short"), "a.jsonl"); err == nil {
		t.Fatal("a name_key of the wrong size must be refused")
	}
}

// The heartbeat lives under the same install prefix, so one sweep erases it too.
func TestStateKeySharesTheInstallPrefix(t *testing.T) {
	const install = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	state, err := formats.StateKey("default", install, "heartbeat.json.age")
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := formats.MirrorKey("default", install, "s", keyA(t), "a.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	prefix := "v1/organization=default/install=" + install + "/"
	if !strings.HasPrefix(state, prefix) || !strings.HasPrefix(mirror, prefix) {
		t.Errorf("state and mirror keys must share the install prefix:\n %s\n %s", state, mirror)
	}
	if _, err := formats.StateKey("default", install, "nested/name"); err == nil {
		t.Error("a state object name containing a slash must be refused")
	}
}

// Nothing about the path may be legible in the key itself.
func TestKeyLeaksNothingAboutThePath(t *testing.T) {
	const username = "jane"
	rel := "projects/-Users-jane-work-secret-project/session.jsonl"
	key, err := formats.MirrorKey("default", "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		"claude-code-transcripts", keyA(t), formats.CanonicalPath(rel, username))
	if err != nil {
		t.Fatal(err)
	}
	leaf := key[strings.LastIndex(key, "/")+1:]

	for _, fragment := range []string{username, "secret-project", "session", "projects", "Users", ".jsonl"} {
		if strings.Contains(leaf, fragment) {
			t.Errorf("mirror name leaks %q: %s", fragment, leaf)
		}
	}
	if len(leaf) != 64+len(".age") {
		t.Errorf("mirror name should be 64 hex chars plus .age, got %q", leaf)
	}
}
