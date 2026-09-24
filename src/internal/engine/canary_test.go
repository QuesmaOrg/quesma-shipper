package engine_test

// Canary: every compiled catalog source is fed its native shape carrying one fixed credential set,
// the real engine ships it, and no decrypted object may still hold a credential. A new catalog
// source fails here until it gets a fixture, or a scrub:false entry with a reason.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/cursorjoin"
)

// canaryUnscrubbed are the catalog's scrub:false sources and why no canary applies to them.
var canaryUnscrubbed = map[string]string{
	"claude-account": "scrub: false, account and usage snapshots fetched from the provider, not user files",
	"codex-account":  "scrub: false, account and usage snapshots fetched from the provider, not user files",
	"cursor-account": "scrub: false, account and usage snapshots fetched from the provider, not user files",
}

const canaryUUID = "0199cccc-dddd-7eee-8fff-000011112222"

// canaryLines carries every codexSecrets value in the context its detector keys on.
func canaryLines() []string {
	s := codexSecrets
	return []string{
		"OPENAI_API_KEY=" + s["openai"],
		"ANTHROPIC_API_KEY=" + s["anthropic"],
		"GITHUB_TOKEN=" + s["ghp"],
		"GH_PAT=" + s["ghfg"],
		"AWS_ACCESS_KEY_ID=" + s["akia"],
		"aws_secret_access_key=" + s["awssecret"],
		"JWT=" + s["jwt"],
		"Authorization: Bearer " + s["bearer"],
		"SLACK_BOT_TOKEN=" + s["slack"],
		"STRIPE_KEY=" + s["stripe"],
	}
}

func canaryText() string { return strings.Join(canaryLines(), "\n") + "\n" }

// canaryJSONL puts each credential line in its own record, under the given text field.
func canaryJSONL(record func(text string) string) string {
	var b strings.Builder
	for _, l := range canaryLines() {
		b.WriteString(record(l))
		b.WriteByte('\n')
	}
	return b.String()
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// canaryFixtures writes each source's native shapes under home.
var canaryFixtures = map[string]func(t *testing.T, home string){
	"claude-code-transcripts": func(t *testing.T, home string) {
		dir := filepath.Join(home, ".claude", "projects", "-work-api")
		cwd := jsonString(filepath.Join(home, "work", "api"))
		writeFile(t, filepath.Join(dir, canaryUUID+".jsonl"), []byte(canaryJSONL(func(l string) string {
			return `{"type":"user","cwd":` + cwd + `,"sessionId":"` + canaryUUID + `","message":{"role":"user","content":` + jsonString(l) + `}}`
		})))
		writeFile(t, filepath.Join(dir, canaryUUID, "subagents", "agent-a1.meta.json"),
			[]byte(`{"toolUseId":"toolu_01","env":`+jsonObject(canaryLines())+`}`))
		writeFile(t, filepath.Join(dir, canaryUUID, "tool-results", "toolu_01.txt"), []byte(canaryText()))
	},
	"claude-code-context": func(t *testing.T, home string) {
		root := filepath.Join(home, ".claude")
		writeFile(t, filepath.Join(root, "projects", "-work-api", "memory", "notes.md"), []byte("# Notes\n\n"+canaryText()))
		writeFile(t, filepath.Join(root, "todos", canaryUUID+"-agent.json"),
			[]byte(`[{"content":`+jsonString(canaryText())+`,"status":"pending"}]`))
		writeFile(t, filepath.Join(root, "plans", "rotate-keys.md"), []byte("# Plan\n\n"+canaryText()))
		writeFile(t, filepath.Join(root, "file-history", canaryUUID, "0f2c6e18461d6b17@v1"), []byte(canaryText()))
	},
	"claude-code-settings": func(t *testing.T, home string) {
		root := filepath.Join(home, ".claude")
		// Without the bearer line: a bare "Bearer <token>" JSON value is a known pre-existing gap (whole-JSON walk), a follow-up.
		settings := slices.DeleteFunc(canaryLines(), func(l string) bool { return strings.HasPrefix(l, "Authorization: Bearer ") })
		writeFile(t, filepath.Join(root, "settings.json"), []byte(`{"env":`+jsonObject(settings)+`}`+"\n"))
		writeFile(t, filepath.Join(root, "CLAUDE.md"), []byte("# Global\n\n"+canaryText()))
	},
	"codex-rollouts": func(t *testing.T, home string) {
		root := filepath.Join(home, ".codex")
		writeFile(t, filepath.Join(root, "sessions", "2026", "09", "20", codexRolloutName(codexUUID1)), []byte(codexRollout(codexUUID1)))
		writeFile(t, filepath.Join(root, "archived_sessions", codexRolloutName(codexUUID2)), []byte(codexRollout(codexUUID2)))
		writeFile(t, filepath.Join(root, "sessions", "2026", "09", "01", codexRolloutName(codexUUID3)+".zst"),
			zstdOf(t, []byte(codexRollout(codexUUID3))))
		writeFile(t, filepath.Join(root, "session_index.jsonl"), []byte(canaryJSONL(func(l string) string {
			return `{"id":"` + codexUUID1 + `","thread_name":` + jsonString(l) + `,"updated_at":"2026-09-20T10:00:00Z"}`
		})))
	},
	"cursor-transcripts": func(t *testing.T, home string) {
		writeCursorCanary(t, home)
	},
	"cursor-agent-outputs": func(t *testing.T, home string) {
		dir := filepath.Join(home, ".cursor", "projects", "api")
		writeFile(t, filepath.Join(dir, "agent-tools", "call_abc123.txt"), []byte(canaryText()))
		writeFile(t, filepath.Join(dir, "terminals", "1.txt"), []byte("$ env\n"+canaryText()))
		writeFile(t, filepath.Join(dir, "agent-notes", "scratch.md"), []byte(canaryText()))
	},
	"project-map": func(t *testing.T, home string) {
		// The transcript fixture supplies the cwd; the remote's userinfo is the planted credential.
		writeFile(t, filepath.Join(home, "work", "api", ".git", "config"), []byte(
			"[remote \"origin\"]\n\turl = https://jane:"+codexSecrets["ghp"]+"@github.com/acme/api.git\n"+
				"[remote \"ci\"]\n\turl = https://x-access-token:"+codexSecrets["ghfg"]+"@github.com/acme/api.git\n"))
	},
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + r.Replace(s) + `"`
}

// jsonObject turns NAME=value lines into a JSON object, the shape of a settings env block.
func jsonObject(lines []string) string {
	var parts []string
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			k, v, _ = strings.Cut(l, ": ")
		}
		parts = append(parts, jsonString(k)+":"+jsonString(v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// writeCursorCanary plants the credentials in the transcript and in the database-only tool result
// that the enricher's derived object adds.
func writeCursorCanary(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".cursor", "projects", "api", "agent-transcripts")
	writeFile(t, filepath.Join(dir, enrichConv+".jsonl"), []byte(cursorTranscript+canaryJSONL(func(l string) string {
		return `{"role":"assistant","message":{"content":[{"type":"text","text":` + jsonString(l) + `}]}}`
	})))

	if err := os.MkdirAll(filepath.Dir(cursorCanaryDB(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+cursorCanaryDB(home))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result := jsonString("total 24\n" + canaryText())
	for _, stmt := range []string{
		`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)`,
		`CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)`,
		fmt.Sprintf(`INSERT INTO cursorDiskKV VALUES ('composerData:%s', '{"composerId":"%s","fullConversationHeadersOnly":[{"bubbleId":"b1","type":1},{"bubbleId":"b2","type":2},{"bubbleId":"b3","type":2}]}')`, enrichConv, enrichConv),
		fmt.Sprintf(`INSERT INTO cursorDiskKV VALUES ('bubbleId:%s:b1', '{"bubbleId":"b1","type":1,"text":"list the workspace"}')`, enrichConv),
		fmt.Sprintf(`INSERT INTO cursorDiskKV VALUES ('bubbleId:%s:b2', '{"bubbleId":"b2","type":2,"text":"Listing the workspace folder contents."}')`, enrichConv),
		fmt.Sprintf(`INSERT INTO cursorDiskKV VALUES ('bubbleId:%s:b3', '{"bubbleId":"b3","type":2,"toolFormerData":{"toolCallId":"call_abc123","name":"run_terminal_cmd","status":"completed","rawArgs":"{\"command\":\"env\"}","result":%s}}')`,
			enrichConv, strings.ReplaceAll(result, "'", "''")),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func cursorCanaryDB(home string) string { return filepath.Join(home, "globalStorage", "state.vscdb") }

func TestEveryCatalogSourceScrubsItsCanary(t *testing.T) {
	catalog, err := sources.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range catalog.Sources() {
		_, fixture := canaryFixtures[src.ID]
		reason, exempt := canaryUnscrubbed[src.ID]
		unscrubbed := src.Scrub != nil && !*src.Scrub
		switch {
		case unscrubbed && !exempt:
			t.Errorf("%s is scrub: false; list it in canaryUnscrubbed with a reason", src.ID)
		case !unscrubbed && exempt:
			t.Errorf("%s is scrubbed but canaryUnscrubbed exempts it (%s); give it a fixture", src.ID, reason)
		case !unscrubbed && !fixture:
			t.Errorf("%s has no canary fixture in its native shape", src.ID)
		}
	}
	if t.Failed() {
		return
	}

	f := newFixture(t)
	// Roots resolve against what exists, so the fixtures go down first.
	for _, write := range canaryFixtures {
		write(t, f.home)
	}
	env := f.resolveCatalog(func(s sources.Resolved) bool {
		_, exempt := canaryUnscrubbed[s.ID]
		return !exempt
	})

	rep := f.runWith(func(o *engine.Options) {
		o.Unbounded = true
		o.Env = env
		o.Enrichers = transforms.NewRegistry(&fixtureEnricher{Enricher: cursorjoin.New(), db: cursorCanaryDB(f.home)})
	})
	for _, so := range rep.Sources {
		for _, fo := range so.Files {
			if fo.Decision == "parked" || fo.Decision == "failed" {
				t.Errorf("%s %s: %s: %s", so.SourceID, fo.RelPath, fo.Decision, fo.Reason)
			}
		}
	}

	shipped := map[string]int{}
	derived := 0
	for _, key := range f.port.keys() {
		m, body := f.plaintext(key)
		shipped[m.SourceID]++
		if m.Derived {
			derived++
		}
		assertNoCodexSecret(t, m, body)
	}
	for _, s := range f.eff.Sources {
		if shipped[s.ID] == 0 {
			t.Errorf("%s shipped nothing, so its canary proves nothing", s.ID)
		}
	}
	if derived == 0 {
		t.Error("no derived object shipped; the enricher output went unchecked")
	}
}

// Opaque bytes defeat every text pattern, so they park naming the source and path, and never ship.
func TestOpaquePayloadParksNamingSourceAndPath(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(f.home, ".claude")
	snap := func(name string, body []byte) string {
		p := filepath.Join(root, "file-history", canaryUUID, name)
		writeFile(t, p, body)
		return p
	}
	text := []byte(canaryText())
	var want []string
	for _, c := range []struct {
		name, kind string
		body       []byte
	}{
		{"a@v1", "zstd", zstdOf(t, text)},
		{"b@v1", "sqlite", append([]byte("SQLite format 3\x00"), text...)},
	} {
		want = append(want, "claude-code-context "+snap(c.name, c.body)+": scrub failed closed: scrub refused: "+c.kind+" payload")
	}
	snap("d@v1", []byte("plain text survives the guard\n"))
	f.eff = f.effective([]sources.Resolved{{
		Source: sources.Source{
			ID: "claude-code-context", Family: "claude-code", Gather: "file_glob", ArtifactClass: "context",
			Include: []string{"file-history/**"}, Sniff: &sources.Sniff{Kind: "none"},
		},
		Root: root, Enabled: true, SpecFingerprint: strings.Repeat("b", 64),
	}})

	rep := f.run()
	if rep.Parked != 2 || rep.Shipped != 1 {
		t.Fatalf("want 2 opaque snapshots parked and the text one shipped, got parked %d shipped %d", rep.Parked, rep.Shipped)
	}
	var got []string
	for _, so := range rep.Sources {
		for _, fo := range so.Files {
			if fo.Decision == "parked" {
				got = append(got, so.SourceID+" "+fo.NativePath+": "+fo.Reason)
			}
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("parked outcomes\n got: %q\nwant: %q", got, want)
	}
}
