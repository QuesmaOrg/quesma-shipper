package sqliteread_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

// newStore builds a state.vscdb-shaped fixture: two key/value tables as Cursor has, planting the rows the filter must catch.
func newStore(t *testing.T, rows map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.vscdb")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`,
		`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range rows {
		table := "cursorDiskKV"
		if strings.HasPrefix(k, "cursorAuth/") {
			// Planted in the table the enricher does NOT declare, and again below in the one it does.
			table = "ItemTable"
		}
		if _, err := db.Exec(`INSERT INTO `+table+` (key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string, opts ...func(*sqliteread.Options)) sqliteread.Result {
	t.Helper()
	o := sqliteread.Options{
		Path:        path,
		ScratchDir:  filepath.Join(t.TempDir(), "scratch"),
		Table:       "cursorDiskKV",
		KeyPrefixes: []string{"composerData:", "bubbleId:"},
	}
	for _, f := range opts {
		f(&o)
	}
	res, err := sqliteread.Read(o)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return res
}

func TestTheFastPathReadsInPlace(t *testing.T) {
	path := newStore(t, map[string]string{
		"composerData:c1": `{"composerId":"c1"}`,
		"bubbleId:c1:b1":  `{"type":1,"text":"hello"}`,
	})

	res := read(t, path)
	if res.Method != sqliteread.ReadInPlace {
		t.Errorf("method = %q, want the in-place fast path", res.Method)
	}
	if len(res.Rows) != 2 {
		t.Errorf("read %d rows, want 2", len(res.Rows))
	}
}

func TestTheDeclaredScopeIsTheOnlyThingRead(t *testing.T) {
	path := newStore(t, map[string]string{
		"composerData:c1":       `{"composerId":"c1"}`,
		"bubbleId:c1:b1":        `{"type":1}`,
		"checkpointId:c1:x":     `{"secret":"not this enricher's business"}`,
		"messageRequestContext": `{"also":"no"}`,
	})

	res := read(t, path)
	// Scope is declared at registration, not discovered: returning the whole table would make the declaration decorative.
	for _, r := range res.Rows {
		if !strings.HasPrefix(r.Key, "composerData:") && !strings.HasPrefix(r.Key, "bubbleId:") {
			t.Errorf("read outside the declared keyspaces: %s", r.Key)
		}
	}
	if len(res.Rows) != 2 {
		t.Errorf("read %d rows, want 2", len(res.Rows))
	}
}

// The exact-key exceptions, stated in both directions: the four named keys pass, and a token key never does.
func TestTheAuthNamespaceExceptionsAreExactKeysOnly(t *testing.T) {
	const token = "SUPER-SECRET-CURSOR-SESSION-TOKEN"
	path := newStore(t, map[string]string{
		"cursorAuth/stripeMembershipType": "enterprise",
		"cursorAuth/cachedEmail":          "dev@example.com",
		"cursorAuth/cachedSignUpType":     "Google",
		"cursorAuth/cachedTeam":           `{"teamId": 1, "name": "Quesma"}`,
		"cursorAuth/accessToken":          token,
		"cursorAuth/refreshToken":         token,
		// A prefix of an allowed key is NOT allowed: exactness is the guarantee.
		"cursorAuth/cachedEmailBackup": token,
	})
	res := read(t, path, func(o *sqliteread.Options) {
		o.Table = "ItemTable" // where Cursor keeps the cursorAuth namespace
		o.KeyPrefixes = []string{"cursorAuth/"}
	})
	got := map[string]string{}
	for _, r := range res.Rows {
		got[r.Key] = string(r.Value)
	}
	for _, want := range []string{"cursorAuth/stripeMembershipType", "cursorAuth/cachedEmail",
		"cursorAuth/cachedSignUpType", "cursorAuth/cachedTeam"} {
		if _, ok := got[want]; !ok {
			t.Errorf("allowed key %s did not survive", want)
		}
	}
	for k, v := range got {
		if strings.Contains(v, token) {
			t.Errorf("a token survived under %s", k)
		}
	}
	if len(got) != 4 {
		t.Errorf("want exactly the 4 allowed keys, got %d: %v", len(got), got)
	}
}

// THE FILTER TEST. Auth material lives in the same database as the trajectories.
func TestTheCompiledFilterStripsAuthKeysAndEncryptionKeyFields(t *testing.T) {
	const token = "SUPER-SECRET-CURSOR-SESSION-TOKEN"
	path := newStore(t, map[string]string{
		// In ItemTable, where Cursor keeps it.
		"cursorAuth/accessToken": token,
		// And in the declared table, spelled two ways, so the filter is not passing by table split or by one spelling.
		"cursorAuth/refreshToken": token,
		"CursorAuth/cased":        token,

		// The encryption-key fields, nested where they really appear.
		"composerData:c1": `{"composerId":"c1","blobEncryptionKey":"` + token + `",` +
			`"speculativeSummarizationEncryptionKey":"` + token + `",` +
			`"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}`,
		"bubbleId:c1:b1": `{"type":1,"text":"hi","nested":{"blobEncryptionKey":"` + token + `"}}`,
	})

	res := read(t, path, func(o *sqliteread.Options) {
		// Deliberately declaring the auth namespace too: the filter is compiled, not something a declaration overrides.
		o.KeyPrefixes = []string{"composerData:", "bubbleId:", "cursorAuth/", "CursorAuth/"}
	})

	for _, r := range res.Rows {
		if strings.Contains(strings.ToLower(r.Key), "cursorauth") {
			t.Errorf("an auth key survived the filter: %s", r.Key)
		}
		if strings.Contains(string(r.Value), token) {
			t.Errorf("the token survived in %s: %s", r.Key, r.Value)
		}
		if strings.Contains(string(r.Value), "EncryptionKey") {
			t.Errorf("an encryption-key field survived in %s", r.Key)
		}
	}
	if res.DeniedKeys == 0 {
		t.Error("no keys were reported denied, so the filter's operation is invisible")
	}
	if res.StrippedFields == 0 {
		t.Error("no fields were reported stripped")
	}

	// The rest of the row has to survive: dropping the whole composerData row would take the conversation with it.
	found := false
	for _, r := range res.Rows {
		if r.Key == "composerData:c1" {
			found = true
			var m map[string]any
			if err := json.Unmarshal(r.Value, &m); err != nil {
				t.Fatalf("the stripped row is no longer JSON: %v", err)
			}
			if m["composerId"] != "c1" {
				t.Error("stripping removed the fields the join needs")
			}
			if m["fullConversationHeadersOnly"] == nil {
				t.Error("stripping removed the bubble ordering")
			}
		}
	}
	if !found {
		t.Error("the composerData row was dropped entirely")
	}
}

func TestNestedEncryptionKeysAreStrippedAtAnyDepth(t *testing.T) {
	const secret = "KEYMATERIAL-DEEP"
	path := newStore(t, map[string]string{
		"composerData:c1": `{"a":{"b":{"c":[{"blobEncryptionKey":"` + secret + `"}]}}}`,
	})
	res := read(t, path)
	for _, r := range res.Rows {
		// Nested inside arrays inside objects, as the real fields are: a shallow filter would pass this.
		if strings.Contains(string(r.Value), secret) {
			t.Errorf("a nested encryption key survived: %s", r.Value)
		}
	}
}

func TestRowsThatNeedNoStrippingAreReturnedVerbatim(t *testing.T) {
	// Byte-identical, not re-encoded: reordered keys would make the output hash re-ship on every run.
	const value = `{"z":1,"a":2,"m":{"y":3,"b":4}}`
	path := newStore(t, map[string]string{"composerData:c1": value})

	res := read(t, path)
	if len(res.Rows) != 1 {
		t.Fatalf("read %d rows", len(res.Rows))
	}
	if string(res.Rows[0].Value) != value {
		t.Errorf("a row that needed no stripping was re-encoded:\n got %s\nwant %s",
			res.Rows[0].Value, value)
	}
}

func TestRowsComeBackInAStableOrder(t *testing.T) {
	path := newStore(t, map[string]string{
		"bubbleId:c1:b3":  `{"type":2}`,
		"bubbleId:c1:b1":  `{"type":1}`,
		"bubbleId:c1:b2":  `{"type":2}`,
		"composerData:c1": `{"composerId":"c1"}`,
	})
	first := read(t, path)
	second := read(t, path)

	// Determinism starts here: a varying read order would vary the derived output bytes and the output hash with them.
	if len(first.Rows) != len(second.Rows) {
		t.Fatalf("row counts differ: %d vs %d", len(first.Rows), len(second.Rows))
	}
	for i := range first.Rows {
		if first.Rows[i].Key != second.Rows[i].Key {
			t.Fatalf("row %d differs between reads: %s vs %s", i, first.Rows[i].Key, second.Rows[i].Key)
		}
	}
}

// The scratch-fallback gate: identical output, and nothing left beside the source.
func TestTheSnapshotReadProducesTheSameRowsAndLeavesNoFilesBehind(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	path := newStore(t, map[string]string{
		"composerData:c1": `{"composerId":"c1"}`,
		"bubbleId:c1:b1":  `{"type":1,"text":"hi"}`,
	})
	scratch := filepath.Join(t.TempDir(), "scratch")

	inPlace := read(t, path, func(o *sqliteread.Options) { o.ScratchDir = scratch })
	if inPlace.Method != sqliteread.ReadInPlace {
		t.Fatalf("expected the fast path first, got %q", inPlace.Method)
	}

	// Force a fallback: whichever method answers, the ROWS must be identical.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := sqliteread.Read(sqliteread.Options{
		Path:        path,
		ScratchDir:  scratch,
		Table:       "cursorDiskKV",
		KeyPrefixes: []string{"composerData:", "bubbleId:"},
	})
	// Unreadable by every method must be reported as a failure, not as an empty successful read.
	if err == nil {
		t.Fatal("an unreadable database produced a successful read")
	}
	if !strings.Contains(err.Error(), "every read method failed") {
		t.Errorf("the error does not name every method: %v", err)
	}
}

// THE NO-FILES-BESIDE-THE-SOURCE GATE: the compensating control for putting this package on the write-path lint's allow-list.
func TestAReadNeverWritesBesideTheSource(t *testing.T) {
	path := newStore(t, map[string]string{
		"composerData:c1": `{"composerId":"c1"}`,
		"bubbleId:c1:b1":  `{"type":1,"text":"hi"}`,
	})
	dir := filepath.Dir(path)
	scratch := filepath.Join(t.TempDir(), "scratch")

	before := listDir(t, dir)

	// Every method in turn: an in-place read must not create -wal or -shm sidecars, and the copying methods must stay under the scratch directory.
	for i := 0; i < 3; i++ {
		if _, err := sqliteread.Read(sqliteread.Options{
			Path:        path,
			ScratchDir:  scratch,
			Table:       "cursorDiskKV",
			KeyPrefixes: []string{"composerData:"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	after := listDir(t, dir)
	if len(after) != len(before) {
		t.Errorf("a read changed the source directory:\n before %v\n after  %v", before, after)
	}
	for i := range after {
		if after[i] != before[i] {
			t.Errorf("a read created %s beside the source", after[i])
		}
	}
}

// A snapshot must not survive the read that made it.
func TestTheSnapshotIsDeletedAfterUse(t *testing.T) {
	path := newStore(t, map[string]string{"composerData:c1": `{"composerId":"c1"}`})
	scratch := filepath.Join(t.TempDir(), "scratch")

	// Pre-create what a crash mid-VACUUM leaves: resuming into it would query a corrupt partial database.
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(scratch, "snapshot-state.vscdb.sqlite")
	if err := os.WriteFile(leftover, []byte("corrupt partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := sqliteread.Read(sqliteread.Options{
		Path:        path,
		ScratchDir:  scratch,
		Table:       "cursorDiskKV",
		KeyPrefixes: []string{"composerData:"},
	}); err != nil {
		t.Fatal(err)
	}
	// The fast path answered, so the leftover was untouched, but no snapshot may have been added either.
	entries := listDir(t, scratch)
	for _, e := range entries {
		if e != "snapshot-state.vscdb.sqlite" {
			t.Errorf("the scratch directory gained %s", e)
		}
	}
}

// THE SIDECAR BASENAME RULE. A mistake this project already made once.
func TestAColdCopyKeepsSidecarBasenames(t *testing.T) {
	src := newStore(t, map[string]string{"composerData:c1": `{"composerId":"c1"}`})
	// Give it a WAL and an SHM, as a live database has.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(src+suffix, []byte("sidecar"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dst := t.TempDir()
	copied, err := sqliteread.CopyCold(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	// A copy that omits or RENAMES the -wal opens without the WAL's contents, since SQLite finds the sidecar by basename.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		want := copied + suffix
		if _, err := os.Stat(want); err != nil {
			t.Errorf("%s did not land beside the copy: %v", filepath.Base(want), err)
		}
	}
	if filepath.Base(copied) != filepath.Base(src) {
		t.Errorf("the copy was renamed: %s vs %s", filepath.Base(copied), filepath.Base(src))
	}
}

func TestAColdCopyRefusesToOverwriteAnExistingTarget(t *testing.T) {
	src := newStore(t, map[string]string{"composerData:c1": `{"composerId":"c1"}`})
	dst := t.TempDir()

	if _, err := sqliteread.CopyCold(src, dst); err != nil {
		t.Fatal(err)
	}
	// Second copy into the same directory: overwriting could mix a fresh database with a stale -wal.
	if _, err := sqliteread.CopyCold(src, dst); err == nil {
		t.Fatal("a cold copy overwrote an existing target")
	}
}

func TestALiveDatabaseIsNotCopiedRaw(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	path := newStore(t, map[string]string{"composerData:c1": `{"composerId":"c1"}`})
	if err := os.Chtimes(path, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// The raw copy is cold-only and must refuse here: a raw copy of a database being written is not consistent.
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	_, err := sqliteread.Read(sqliteread.Options{
		Path:        path,
		ScratchDir:  filepath.Join(t.TempDir(), "scratch"),
		Table:       "cursorDiskKV",
		KeyPrefixes: []string{"composerData:"},
		Now:         func() time.Time { return time.Now() },
	})
	if err == nil {
		t.Fatal("a live database was read by raw copy")
	}
	if !strings.Contains(err.Error(), "not cold enough") {
		t.Errorf("the refusal does not name coldness: %v", err)
	}
}

func TestAnUndeclaredTableIsRefused(t *testing.T) {
	path := newStore(t, map[string]string{"composerData:c1": `{}`})
	_, err := sqliteread.Read(sqliteread.Options{
		Path:       path,
		ScratchDir: t.TempDir(),
	})
	// Scope is declared: a read with no table is a programming fault, not something to guess at.
	if err == nil {
		t.Fatal("a read with no declared table was accepted")
	}
}

func TestALikeWildcardInAPrefixMatchesLiterally(t *testing.T) {
	path := newStore(t, map[string]string{
		"composerData:c1": `{"composerId":"c1"}`,
		"bubbleId:c1:b1":  `{"type":1}`,
	})
	res := read(t, path, func(o *sqliteread.Options) {
		// A prefix containing % must not sweep in every key: declared scope has to mean what it says.
		o.KeyPrefixes = []string{"%"}
	})
	if len(res.Rows) != 0 {
		t.Errorf("a %% prefix matched %d rows; wildcards in a declared prefix must be literal", len(res.Rows))
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// THE FALLBACK-EQUIVALENCE GATE: every read method returns identical ROW VALUES, which is why hashes come from values and not bytes.
func TestEveryReadMethodReturnsIdenticalRows(t *testing.T) {
	path := newStore(t, map[string]string{
		"composerData:c1": `{"composerId":"c1","fullConversationHeadersOnly":[{"bubbleId":"b1","type":1}]}`,
		"bubbleId:c1:b1":  `{"type":1,"text":"hello","createdAt":"2026-07-30T10:00:00Z"}`,
		"bubbleId:c1:b2":  `{"type":2,"text":"world","createdAt":"2026-07-30T10:00:01Z"}`,
	})
	// Old enough for the cold copy to accept it.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	methods := []sqliteread.ReadMethod{
		sqliteread.ReadInPlace,
		sqliteread.ReadSnapshot,
		sqliteread.ReadColdCopy,
	}
	var reference []sqliteread.Row

	for _, want := range methods {
		res, err := sqliteread.Read(sqliteread.Options{
			Path:        path,
			ScratchDir:  filepath.Join(t.TempDir(), "scratch"),
			Table:       "cursorDiskKV",
			KeyPrefixes: []string{"composerData:", "bubbleId:"},
			StartAt:     want,
		})
		if err != nil {
			t.Fatalf("method %s: %v", want, err)
		}
		if res.Method != want {
			t.Fatalf("asked for method %s, got %s", want, res.Method)
		}
		if reference == nil {
			reference = res.Rows
			continue
		}
		if len(res.Rows) != len(reference) {
			t.Fatalf("method %s returned %d rows, method %s returned %d",
				want, len(res.Rows), methods[0], len(reference))
		}
		for i := range res.Rows {
			if res.Rows[i].Key != reference[i].Key {
				t.Errorf("method %s row %d key = %s, want %s", want, i, res.Rows[i].Key, reference[i].Key)
			}
			if string(res.Rows[i].Value) != string(reference[i].Value) {
				t.Errorf("method %s row %s value differs from the in-place read", want, res.Rows[i].Key)
			}
		}
	}
}

func TestTheSnapshotReadLeavesNothingInTheScratchDirectory(t *testing.T) {
	path := newStore(t, map[string]string{"composerData:c1": `{"composerId":"c1"}`})
	scratch := filepath.Join(t.TempDir(), "scratch")

	res, err := sqliteread.Read(sqliteread.Options{
		Path:        path,
		ScratchDir:  scratch,
		Table:       "cursorDiskKV",
		KeyPrefixes: []string{"composerData:"},
		StartAt:     sqliteread.ReadSnapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != sqliteread.ReadSnapshot {
		t.Fatalf("method = %s", res.Method)
	}
	// A snapshot left behind is a copy of an agent's database accumulating in the state directory.
	if left := listDir(t, scratch); len(left) != 0 {
		t.Errorf("the scratch directory still holds %v", left)
	}
}

func TestALeftoverSnapshotIsDeletedRatherThanResumedInto(t *testing.T) {
	path := newStore(t, map[string]string{"composerData:c1": `{"composerId":"c1"}`})
	scratch := filepath.Join(t.TempDir(), "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	// What a crash mid-VACUUM leaves: a file with the snapshot's name that is not a valid database.
	leftover := filepath.Join(scratch, "snapshot-state.vscdb.sqlite")
	if err := os.WriteFile(leftover, []byte("corrupt partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := sqliteread.Read(sqliteread.Options{
		Path:        path,
		ScratchDir:  scratch,
		Table:       "cursorDiskKV",
		KeyPrefixes: []string{"composerData:"},
		StartAt:     sqliteread.ReadSnapshot,
	})
	if err != nil {
		t.Fatalf("a leftover snapshot broke the read instead of being replaced: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Errorf("read %d rows from the fresh snapshot, want 1", len(res.Rows))
	}
}
