package engine_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/accountprobe"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/cursorjoin"
)

type fixtureClaude struct {
	*accountprobe.Claude
	db string
}

func (f *fixtureClaude) DBCandidates() []string { return []string{f.db} }

// The account probe's derived object must survive the REAL seal path: its manifest carries
// enricher provenance, and a schema refusal at seal costs the account object silently.
func TestAccountProbeDerivedObjectShipsThroughTheEngine(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.home, ".claude", "projects", "p1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s1.jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(f.home, ".claude.json")
	if err := os.WriteFile(store, []byte(`{"oauthAccount":{"emailAddress":"dev@example.com","organizationType":"claude_max","organizationRateLimitTier":"default_claude_max_20x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	o := f.opts()
	o.Plan.Sources = []sources.Resolved{{
		Source: sources.Source{
			ID: "claude-code-transcripts", Family: "claude-code", Gather: "file_glob",
			ArtifactClass: "trajectory",
			Include:       []string{"projects/**/*.jsonl"},
			Enrichers:     map[string]bool{"claude-account": true},
		},
		Root:            filepath.Join(f.home, ".claude"),
		Enabled:         true,
		SpecFingerprint: strings.Repeat("a", 64),
	}}
	o.Enrichers = transforms.NewRegistry(&fixtureClaude{Claude: accountprobe.NewClaude(), db: store})
	env, err := sources.OSEnv()
	if err != nil {
		t.Fatal(err)
	}
	o.Env = env
	o.Recipients = []age.Recipient{f.unit.Recipient()}
	rep := runEnrich(t, f, o)
	found := false
	for _, s := range rep.Sources {
		if s.Enriched > 0 {
			found = true
		}
		for _, f := range s.Files {
			if f.Derived && f.Decision != "shipped" {
				t.Errorf("derived object %s: %s (%s)", f.NativePath, f.Decision, f.Reason)
			}
		}
	}
	if !found {
		t.Fatal("no derived account object shipped")
	}
}

type fixtureCursorAccount struct {
	*accountprobe.Cursor
	db string
}

func (f *fixtureCursorAccount) DBCandidates() []string { return []string{f.db} }

// An installed, logged-in, IDLE Cursor: nothing is staged, so this is the gate on the NeedsUnits
// split. The probe ships anyway, its input being the auth store, while the join stays quiet.
func TestCursorAccountShipsWithoutAnyStagedTranscript(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.home, ".cursor", "projects"), 0o700); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(f.home, "globalStorage", "state.vscdb")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`,
		`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`,
		`INSERT INTO ItemTable (key, value) VALUES
			('cursorAuth/stripeMembershipType', 'business'),
			('cursorAuth/cachedEmail', 'dev@example.com'),
			('cursorAuth/cachedSignUpType', 'Google'),
			('cursorAuth/cachedTeam', '{"name":"platform"}'),
			('cursorAuth/accessToken', '` + sessionToken + `')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	o := f.opts()
	o.Plan.Sources = []sources.Resolved{{
		Source: sources.Source{
			ID: "cursor-transcripts", Family: "cursor", Gather: "file_glob",
			ArtifactClass: "trajectory",
			Include:       []string{"**/agent-transcripts/**/*.jsonl"},
			Enrichers:     map[string]bool{"cursor-account": true, "cursor-transcript-join": true},
		},
		Root:            filepath.Join(f.home, ".cursor", "projects"),
		Enabled:         true,
		SpecFingerprint: strings.Repeat("d", 64),
	}}
	o.Enrichers = transforms.NewRegistry(
		&fixtureCursorAccount{Cursor: accountprobe.NewCursor(), db: dbPath},
		&fixtureEnricher{Enricher: cursorjoin.New(), db: dbPath},
	)
	env, err := sources.OSEnv()
	if err != nil {
		t.Fatal(err)
	}
	o.Env = env
	o.Recipients = []age.Recipient{f.unit.Recipient()}

	derived := func(rep engine.Report) []engine.FileOutcome {
		var out []engine.FileOutcome
		for _, s := range rep.Sources {
			for _, fo := range s.Files {
				if fo.Derived {
					out = append(out, fo)
				}
			}
		}
		return out
	}

	first := derived(runEnrich(t, f, o))
	if len(first) != 1 {
		t.Fatalf("want exactly one derived object (the account), got %d: %+v", len(first), first)
	}
	if !strings.HasSuffix(first[0].NativePath, ".account.enriched.json") {
		t.Errorf("derived object is not the account: %s", first[0].NativePath)
	}
	if first[0].Decision != auditlog.DecisionShipped {
		t.Errorf("account object: %s (%s)", first[0].Decision, first[0].Reason)
	}

	// The probe runs again on every flush; the unchanged output hash keeps it from uploading.
	second := derived(runEnrich(t, f, o))
	if len(second) != 1 || second[0].Decision != auditlog.DecisionUnchanged {
		t.Fatalf("second flush: want one unchanged derived outcome, got %+v", second)
	}
}
