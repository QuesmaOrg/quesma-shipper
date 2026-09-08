package engine_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	_ "modernc.org/sqlite"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/cursorjoin"
)

// End-to-end gates: a raw + derived pair through the real loop, the derived object taking the
// same scrub/seal/send path, and the invariant that matters most: NO SQLite rows in the sink.

const enrichConv = "5d1f7b3e-9a2c-4e8f-b1d0-3c4a5b6c7d8e"

// cursorTranscript is the raw side, in the shape the survey found exhaustive.
const cursorTranscript = `{"role":"user","message":{"content":[{"type":"text","text":"<user_query>\nlist the workspace\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Listing the workspace folder contents."},{"type":"tool_use","name":"Shell","input":{"command":"ls -la /work/api"}}]}}
{"type":"turn_ended","status":"success"}
`

// Markers that must never appear in any object; all three are planted in the fixture database.
const (
	unusedRowMarker = "UNUSED_STORE_FIELD_MUST_NOT_SHIP"
	sessionToken    = "CURSOR-SESSION-TOKEN-MUST-NOT-SHIP"
	blobKey         = "BLOB-ENCRYPTION-KEY-MUST-NOT-SHIP"
)

// cursorFixture writes a Cursor-shaped store and transcript into the fixture's home.
func cursorFixture(t *testing.T, f *fixture) (dbPath string) {
	t.Helper()

	// The transcript, where the catalog's globs will find it.
	dir := filepath.Join(f.home, ".cursor", "projects", "api", "agent-transcripts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, enrichConv+".jsonl"), []byte(cursorTranscript), 0o600); err != nil {
		t.Fatal(err)
	}

	// The store, with auth material and unused fields planted in it.
	dbPath = filepath.Join(f.home, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
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

	rows := []struct{ table, key, value string }{
		{"ItemTable", "cursorAuth/accessToken", sessionToken},
		{"cursorDiskKV", "cursorAuth/refreshToken", sessionToken},
		{
			"cursorDiskKV", "composerData:" + enrichConv,
			fmt.Sprintf(`{"composerId":%q,"blobEncryptionKey":%q,"%s":"x",
				"fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2}]}`,
				enrichConv, blobKey, unusedRowMarker),
		},
		{"cursorDiskKV", "bubbleId:" + enrichConv + ":b1", `{"bubbleId":"b1","type":1,"text":"list the workspace"}`},
		{"cursorDiskKV", "bubbleId:" + enrichConv + ":b2", `{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}`},
		{
			"cursorDiskKV", "bubbleId:" + enrichConv + ":b3",
			`{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
				"name":"run_terminal_cmd","status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
				"result":"total 24\n-rw-r--r-- 1 jane staff 812 main.go"}}`,
		},
		// A checkpoint row, outside the declared keyspaces entirely.
		{"cursorDiskKV", "checkpointId:" + enrichConv + ":x", `{"` + unusedRowMarker + `":"y"}`},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO `+r.table+` (key, value) VALUES (?, ?)`, r.key, r.value); err != nil {
			t.Fatalf("insert %s: %v", r.key, err)
		}
	}
	return dbPath
}

// cursorSource is the catalog source the enricher attaches to.
func cursorSource(f *fixture, enricherOn bool) sources.Resolved {
	return sources.Resolved{
		Source: sources.Source{
			ID:            "cursor-transcripts",
			Family:        "cursor",
			Gather:        "file_glob",
			ArtifactClass: "trajectory",
			Include:       []string{"**/agent-transcripts/**/*.jsonl"},
			Enrichers:     map[string]bool{"cursor-transcript-join": enricherOn},
			Sniff:         &sources.Sniff{Kind: "jsonl", MaxScanBytes: 65536},
		},
		Root:            filepath.Join(f.home, ".cursor", "projects"),
		Enabled:         true,
		SpecFingerprint: strings.Repeat("c", 64),
	}
}

// enrichOpts builds engine options with the enricher registry wired at the fixture's database.
func enrichOpts(t *testing.T, f *fixture, dbPath string, enricherOn bool) engine.Options {
	t.Helper()
	o := f.opts()
	o.Plan.Sources = []sources.Resolved{cursorSource(f, enricherOn)}
	o.Enrichers = transforms.NewRegistry(&fixtureEnricher{Enricher: cursorjoin.New(), db: dbPath})
	env, err := sources.OSEnv()
	if err != nil {
		t.Fatal(err)
	}
	o.Env = env
	o.Recipients = []age.Recipient{f.unit.Recipient()}
	return o
}

// fixtureEnricher is the real join with only the database LOCATION overridden: the join, the read
// ladder and the compiled filter must stay the shipping code.
type fixtureEnricher struct {
	*cursorjoin.Enricher
	db string
}

func (f *fixtureEnricher) DBCandidates() []string { return []string{f.db} }

func runEnrich(t *testing.T, f *fixture, o engine.Options) engine.Report {
	t.Helper()
	rep, err := engine.Run(context.Background(), f.store, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return rep
}

// THE PAIR GATE: raw and derived ship together, and the derived object names its input.
func TestARawAndDerivedPairShipTogether(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)

	rep := runEnrich(t, f, enrichOpts(t, f, db, true))
	if rep.Shipped != 2 {
		t.Fatalf("expected a raw + derived pair, shipped %d: %+v", rep.Shipped, rep.Sources)
	}

	var rawManifest, derivedManifest transforms.Manifest
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		m, _, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if m.Derived {
			derivedManifest = m
		} else {
			rawManifest = m
		}
	}
	if rawManifest.SourceHash == "" {
		t.Fatal("no raw object shipped")
	}
	if !derivedManifest.Derived {
		t.Fatal("no derived object shipped")
	}

	// derived_from carries the raw object's source hash, the only link downstream can verify.
	if len(derivedManifest.DerivedFrom) != 1 || derivedManifest.DerivedFrom[0] != rawManifest.SourceHash {
		t.Errorf("derived_from = %v, want the raw source_hash %s",
			derivedManifest.DerivedFrom, rawManifest.SourceHash)
	}
	if derivedManifest.Enricher == nil ||
		derivedManifest.Enricher.ID != "cursor-transcript-join" || derivedManifest.Enricher.Version != 4 {
		t.Errorf("enricher = %+v", derivedManifest.Enricher)
	}
	if derivedManifest.EnrichStatus != string(transforms.StatusOK) {
		t.Errorf("enrich_status = %q", derivedManifest.EnrichStatus)
	}

	// The rows never ship, so DB provenance is the only account of where the fields came from.
	p := derivedManifest.DBProvenance
	if p == nil || p.ReadMethod == "" || p.RowsRead == 0 || len(p.Keyspaces) == 0 {
		t.Fatalf("db_provenance is incomplete: %+v", p)
	}
	if !strings.Contains(p.DBPath, "state.vscdb") {
		t.Errorf("db_provenance path = %q", p.DBPath)
	}
	// A real path on someone's machine, so the username placeholder applies here too.
	if strings.Contains(p.DBPath, "/Users/"+os.Getenv("USER")+"/") {
		t.Errorf("db_provenance leaks the username: %q", p.DBPath)
	}
}

// THE NO-ROWS GATE. The single most important invariant.
func TestNoSQLiteRowsReachTheSink(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)

	runEnrich(t, f, enrichOpts(t, f, db, true))

	if len(f.port.keys()) == 0 {
		t.Fatal("nothing shipped, so this test proves nothing")
	}
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)

		// The ciphertext itself first: the decrypted payload alone would miss a metadata leak.
		if bytesContain(obj.Body, sessionToken, blobKey, unusedRowMarker) {
			t.Errorf("%s: a store secret or unused row field is in the ciphertext", k)
		}
		for mk, mv := range obj.Metadata {
			if containsAny(mv, sessionToken, blobKey, unusedRowMarker) {
				t.Errorf("%s: metadata %s leaks a store value", k, mk)
			}
		}

		m, payload, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		// The session token shares the database, so this pins the key deny into the shipping path.
		if containsAny(string(payload), sessionToken) {
			t.Errorf("%s: a Cursor session token reached the sink", k)
		}
		if containsAny(string(payload), blobKey) {
			t.Errorf("%s: a blob encryption key reached the sink", k)
		}
		// The derived object is a join of named fields, never a dump of rows.
		if containsAny(string(payload), unusedRowMarker) {
			t.Errorf("%s: an unused store row field reached the sink", k)
		}
		// And nothing may claim to be a database row.
		if m.Gather == "sqlite_rows" || strings.Contains(m.NativePath, "state.vscdb") {
			t.Errorf("%s: an object claims to carry database rows: gather=%q path=%q",
				k, m.Gather, m.NativePath)
		}
	}
}

// THE OUTPUT-HASH PAIR: unchanged output does not re-ship, changed output does.
func TestTheDerivedObjectReShipsOnlyWhenItsOutputChanges(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)

	first := runEnrich(t, f, enrichOpts(t, f, db, true))
	if first.Shipped != 2 {
		t.Fatalf("first run shipped %d, want 2", first.Shipped)
	}
	keysAfterFirst := len(f.port.keys())

	// Second run, nothing changed: determinism means the output hash matches and nothing uploads.
	f.reopen()
	second := runEnrich(t, f, enrichOpts(t, f, db, true))
	if second.Shipped != 0 {
		t.Errorf("an unchanged run re-shipped %d objects", second.Shipped)
	}
	if len(f.port.keys()) != keysAfterFirst {
		t.Errorf("an unchanged run created new keys: %d -> %d", keysAfterFirst, len(f.port.keys()))
	}

	// A DB-side-only change: the transcript is byte-identical, so only the derived object may move.
	updateBubble(t, db, "bubbleId:"+enrichConv+":b3",
		`{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
			"name":"run_terminal_cmd","status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
			"result":"A LATE RESULT ARRIVED"}}`)

	f.reopen()
	third := runEnrich(t, f, enrichOpts(t, f, db, true))
	if third.Shipped != 1 {
		t.Fatalf("a DB-side-only change shipped %d objects, want exactly the derived one: %+v",
			third.Shipped, third.Sources)
	}
	// Onto the SAME key: path-derived naming makes a revision a version, not a second object.
	if len(f.port.keys()) != keysAfterFirst {
		t.Errorf("the re-ship created a new key: %d -> %d", keysAfterFirst, len(f.port.keys()))
	}

	found := false
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		m, payload, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if m.Derived && strings.Contains(string(payload), "A LATE RESULT ARRIVED") {
			found = true
		}
	}
	if !found {
		t.Error("the late result never reached the sink")
	}
}

// THE DISABLED-ENRICHER GATE: byte-identical to a raw-only run.
func TestDisablingTheEnricherLeavesAByteIdenticalRawRun(t *testing.T) {
	withEnricher := newFixture(t)
	dbA := cursorFixture(t, withEnricher)
	runEnrich(t, withEnricher, enrichOpts(t, withEnricher, dbA, true))

	without := newFixture(t)
	dbB := cursorFixture(t, without)
	runEnrich(t, without, enrichOpts(t, without, dbB, false))

	// Disabling must change exactly one thing, the derived object, and leave raw collection alone.
	rawWith := rawManifests(t, withEnricher)
	rawWithout := rawManifests(t, without)

	if len(rawWithout) != 1 {
		t.Fatalf("the disabled run shipped %d raw objects, want 1", len(rawWithout))
	}
	if len(rawWith) != 1 {
		t.Fatalf("the enriched run shipped %d raw objects, want 1", len(rawWith))
	}
	if rawWith[0].SourceHash != rawWithout[0].SourceHash {
		t.Error("enabling the enricher changed the raw object's source hash")
	}
	// Compared without the temp directory: the two runs have different homes.
	if filepath.Base(rawWith[0].NativePath) != filepath.Base(rawWithout[0].NativePath) {
		t.Error("enabling the enricher changed the raw object's path")
	}

	// And no derived object at all when disabled.
	for _, k := range without.port.keys() {
		obj, _ := without.port.get(k)
		m, _, err := transforms.Open(obj.Body, without.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if m.Derived {
			t.Errorf("a disabled enricher still produced %s", k)
		}
	}
}

// A mismatch: raw ships, no derived object, and the alarm reaches the report.
func TestAMismatchShipsRawOnlyAndRaisesTheAlarm(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)

	// Break the join the way vendor drift would, MID-stream: divergence after the last matched
	// event is tolerated as a tail rather than counted as drift.
	updateBubble(t, db, "bubbleId:"+enrichConv+":b2",
		`{"bubbleId":"b2","type":2,"text":"a completely different sentence about nothing"}`)

	rep := runEnrich(t, f, enrichOpts(t, f, db, true))

	// Raw ships regardless, which keeps a drifted join from becoming a collection outage.
	if rep.Shipped != 1 {
		t.Fatalf("shipped %d, want the raw object only: %+v", rep.Shipped, rep.Sources)
	}
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		m, _, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if m.Derived {
			t.Errorf("a mismatched join still shipped a derived object: %s", k)
		}
	}

	// The alarm must reach the report at both levels: a mismatch means data goes uncollected.
	if rep.EnrichMismatch == 0 {
		t.Error("the run-level mismatch counter is zero")
	}
	var src engine.SourceOutcome
	for _, s := range rep.Sources {
		if s.SourceID == "cursor-transcripts" {
			src = s
		}
	}
	if src.EnrichMismatch == 0 {
		t.Error("the per-source mismatch counter is zero")
	}
	if len(src.EnrichNotes) == 0 {
		t.Error("no note explains the mismatch")
	}
}

// preview computes the derived object and uploads nothing.
func TestPreviewComputesTheDerivedObjectWithoutUploading(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)

	o := enrichOpts(t, f, db, true)
	o.DryRun = true
	rep := runEnrich(t, f, o)

	if len(f.port.keys()) != 0 {
		t.Errorf("preview uploaded %v", f.port.keys())
	}
	// Preview must cover the derived object too: it is the one built from a database.
	var derived bool
	for _, s := range rep.Sources {
		for _, fo := range s.Files {
			if strings.HasSuffix(fo.NativePath, ".enriched.jsonl") {
				derived = true
			}
		}
	}
	if !derived {
		t.Error("preview did not report the derived object it would have shipped")
	}
}

// A missing database is not an alarm.
func TestNoDatabaseShipsRawOnlyWithoutAnAlarm(t *testing.T) {
	f := newFixture(t)
	cursorFixture(t, f)

	rep := runEnrich(t, f, enrichOpts(t, f, filepath.Join(f.home, "nope", "state.vscdb"), true))
	if rep.Shipped != 1 {
		t.Fatalf("shipped %d, want the raw object only", rep.Shipped)
	}
	// An install with no store has nothing to derive from; counting it would bury the real alarm.
	if rep.EnrichMismatch != 0 {
		t.Errorf("a missing database raised %d mismatches", rep.EnrichMismatch)
	}
}

// The derived object goes through the same redaction as a raw file.
func TestTheDerivedPayloadIsScrubbed(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)

	// A planted secret in the tool RESULT: a store-side field that can only reach the sink through
	// the derived object, so it is what an unredacted derived path would leak.
	const planted = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	updateBubble(t, db, "bubbleId:"+enrichConv+":b3",
		`{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
			"name":"run_terminal_cmd","status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
			"result":"exported GITHUB_TOKEN=`+planted+`"}}`)

	runEnrich(t, f, enrichOpts(t, f, db, true))

	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		_, payload, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), planted) {
			t.Errorf("%s: a secret in a tool result shipped unredacted", k)
		}
	}
}

// The derived object is flagged in plaintext metadata, so an erasure sweep needs only a HEAD.
func TestTheDerivedObjectIsFlaggedInMetadata(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)
	runEnrich(t, f, enrichOpts(t, f, db, true))

	var flagged bool
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		m, _, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if !m.Derived {
			if obj.Metadata["derived"] != "" {
				t.Errorf("%s: a raw object is flagged derived", k)
			}
			continue
		}
		flagged = true
		if obj.Metadata["derived"] != "true" {
			t.Errorf("%s: derived object is not flagged derived=true (metadata %v)", k, obj.Metadata)
		}
		if obj.Metadata["artifact-class"] == "" {
			t.Errorf("%s: derived object carries no artifact-class metadata", k)
		}
	}
	if !flagged {
		t.Fatal("no derived object shipped")
	}
}

// The recompute window: an unchanged transcript is still read while it is recent.
func TestAnUnchangedTranscriptIsStillEnrichedInsideTheRecomputeWindow(t *testing.T) {
	f := newFixture(t)
	db := cursorFixture(t, f)

	runEnrich(t, f, enrichOpts(t, f, db, true))
	f.reopen()

	// The transcript is untouched and the store gained a late result: without the recompute window
	// its size and mtime never move, so it would never be read again.
	updateBubble(t, db, "bubbleId:"+enrichConv+":b3",
		`{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123",
			"name":"run_terminal_cmd","status":"completed","rawArgs":"{\"command\":\"ls -la /work/api\"}",
			"result":"LATE"}}`)

	rep := runEnrich(t, f, enrichOpts(t, f, db, true))
	if rep.Shipped != 1 {
		t.Fatalf("shipped %d, want the derived object: %+v", rep.Shipped, rep.Sources)
	}
}

// --- helpers ---------------------------------------------------------------

func updateBubble(t *testing.T, dbPath, key, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE cursorDiskKV SET value = ? WHERE key = ?`, value, key); err != nil {
		t.Fatal(err)
	}
	// Touch the file so a coldness check cannot mistake it for stale.
	if err := os.Chtimes(dbPath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func rawManifests(t *testing.T, f *fixture) []transforms.Manifest {
	t.Helper()
	var out []transforms.Manifest
	for _, k := range f.port.keys() {
		obj, _ := f.port.get(k)
		m, _, err := transforms.Open(obj.Body, f.unit.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if !m.Derived {
			out = append(out, m)
		}
	}
	return out
}

func bytesContain(b []byte, needles ...string) bool {
	return containsAny(string(b), needles...)
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}
