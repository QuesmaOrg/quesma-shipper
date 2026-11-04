package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func spillRequest(t testing.TB, root string) Request {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	spill, _ := c.Source("opencode-tool-outputs")
	transcripts, _ := c.Source("opencode-sessions")
	return Request{Source: Resolved{Source: spill, Root: root, Enabled: true},
		All: []Resolved{{Source: transcripts, Root: root, Enabled: true}}, Ignore: c.RepoFilter()}
}

func writeSpill(t testing.TB, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func spillStore(t testing.TB, root, dbName, layout, cwd string, refs []string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, dbName))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := `CREATE TABLE session(id TEXT,parent_id TEXT,directory TEXT,title TEXT,version TEXT,time_created INTEGER,time_updated INTEGER);
CREATE TABLE message(id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
CREATE TABLE part(id TEXT,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);`
	insert := `INSERT INTO session VALUES ('s',NULL,?,'fixture','1',1,1)`
	if layout != "v1" {
		schema += `CREATE TABLE session_message(id TEXT,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,time_updated INTEGER,data TEXT);`
	}
	if layout == "split" {
		schema = `CREATE TABLE session_message(id TEXT,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,time_updated INTEGER,data TEXT);
CREATE TABLE session_v2(id TEXT,parent_id TEXT,fork_session_id TEXT,fork_boundary TEXT,directory TEXT,title TEXT,version TEXT,time_created INTEGER,time_updated INTEGER,cost REAL,tokens_input INTEGER,tokens_output INTEGER,tokens_reasoning INTEGER,tokens_cache_read INTEGER,tokens_cache_write INTEGER);`
		insert = `INSERT INTO session_v2 VALUES ('s',NULL,NULL,NULL,?,'fixture','2',1,1,0,0,0,0,0,0)`
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insert, cwd); err != nil {
		t.Fatal(err)
	}
	for i, ref := range refs {
		tool := map[string]any{"type": "tool", "state": map[string]any{"status": "completed", "metadata": map[string]any{"truncated": true, "outputPath": ref}}}
		var data any = tool
		query := `INSERT INTO part VALUES (?,'m','s',1,1,?)`
		if layout != "v1" {
			data = map[string]any{"content": []any{tool}}
			query = `INSERT INTO session_message VALUES (?,'s','assistant',1,1,1,?)`
		}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(query, i, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenCodeReferencedSpillsLayoutsAndIdentity(t *testing.T) {
	for _, layout := range []string{"v1", "shared", "split"} {
		t.Run(layout, func(t *testing.T) {
			root := t.TempDir()
			ref := filepath.Join(root, "tool-output", "tool_0123456789abXYZ")
			writeSpill(t, root, "tool-output/tool_0123456789abXYZ", "full output\n")
			writeSpill(t, root, "tool-output/tool_unreferenced", "must not ship")
			for _, db := range []string{"opencode.db", "opencode-stable.db"} {
				spillStore(t, root, db, layout, "/work/api", []string{ref, ref})
			}
			req := spillRequest(t, root)
			d, err := (opencodeToolOutputs{}).Discover(req)
			if err != nil || d.Health != Collected || len(d.Candidates) != 1 {
				t.Fatalf("discovery: %+v %v", d, err)
			}
			c := d.Candidates[0]
			if c.RelPath != "tool-output/tool_0123456789abXYZ" || c.Identity != "" || c.CWD != "/work/api" {
				t.Fatalf("root-relative file identity: %+v", c)
			}
			p, err := c.Load(context.Background())
			if err != nil || string(p.Bytes) != "full output\n" {
				t.Fatalf("load: %+v %v", p, err)
			}
		})
	}
}

func TestOpenCodeSpillReferenceGrammar(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for _, rel := range []string{"tool-output/tool_good", "tool-output/tool_unreferenced", "tool-output/nested/tool_hidden", "auth.json", "storage/session/project/session.json"} {
		writeSpill(t, root, rel, "fixture")
	}
	writeSpill(t, outside, "tool_secret", "outside")
	refs := []string{filepath.Join(root, "tool-output", "tool_good"),
		filepath.Join(outside, "tool_secret"), filepath.Join(root, "auth.json"),
		filepath.Join(root, "tool-output", "nested", "tool_hidden"),
		root + "/tool-output/../tool-output/tool_unreferenced", "tool-output/tool_unreferenced",
		filepath.Join(root, "tool-output", "tool_missing")}
	spillStore(t, root, "opencode.db", "v1", "/work/api", refs)
	d, err := (opencodeToolOutputs{}).Discover(spillRequest(t, root))
	if err != nil || len(d.Candidates) != 1 || d.Candidates[0].RelPath != "tool-output/tool_good" {
		t.Fatalf("escaped reference grammar: %+v %v", d, err)
	}
	tool := `{"type":"tool","state":{"metadata":{"truncated":true,"outputPath":"/safe/tool-output/tool_good"}}}`
	for _, rec := range []string{
		`{"type":"part","data":` + tool + `}`,
		`{"type":"session_message","message_type":"assistant","data":{"content":[` + tool + `]}}`,
	} {
		if got := opencodeOutputPaths([]byte(rec)); !slices.Equal(got, []string{"/safe/tool-output/tool_good"}) {
			t.Fatalf("typed metadata: %v", got)
		}
		for _, bad := range []string{strings.ReplaceAll(rec, `"tool"`, `"text"`), strings.ReplaceAll(rec, `true`, `false`), strings.ReplaceAll(rec, `"part"`, `"message"`), strings.ReplaceAll(rec, `"assistant"`, `"user"`), `{broken`} {
			if bad == rec {
				continue
			}
			if got := opencodeOutputPaths([]byte(bad)); len(got) != 0 {
				t.Fatalf("untyped metadata collected: %s => %v", bad, got)
			}
		}
	}
}

func TestOpenCodeSpillSafetyAndCaps(t *testing.T) {
	for _, change := range []string{"denied", "excluded", "oversize", "leaf symlink", "directory symlink", "disabled transcripts", "ignored repo", "transcript cap", "cancelled", "load symlink", "load parent symlink", "load deny", "load growth"} {
		t.Run(change, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			rel := "tool-output/tool_fixture"
			writeSpill(t, root, rel, "full text")
			writeSpill(t, outside, "tool_fixture", "outside text")
			cwd := t.TempDir()
			spillStore(t, root, "opencode.db", "v1", cwd, []string{filepath.Join(root, filepath.FromSlash(rel))})
			req := spillRequest(t, root)
			req.Deny = &List{}
			swap := func(parent bool) {
				from, to := filepath.Join(root, filepath.FromSlash(rel)), filepath.Join(outside, "tool_fixture")
				if parent {
					from, to = filepath.Dir(from), outside
				}
				if err := os.Rename(from, from+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(to, from); err != nil {
					t.Fatal(err)
				}
			}
			switch change {
			case "denied":
				req.Deny.patterns = []string{normalize(filepath.Join(root, filepath.FromSlash(rel)))}
			case "excluded":
				req.Source.Exclude = []string{"tool-output/**"}
			case "oversize":
				req.Source.MaxFileBytes = 2
			case "leaf symlink":
				swap(false)
			case "directory symlink":
				swap(true)
			case "disabled transcripts":
				req.All[0].Enabled = false
			case "ignored repo":
				writeSpill(t, cwd, ".notrajectories", "")
			case "transcript cap":
				req.All[0].MaxFileBytes = 2
			case "cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				req.Context = ctx
			}
			d, err := (opencodeToolOutputs{}).Discover(req)
			if strings.HasPrefix(change, "load ") {
				if err != nil || len(d.Candidates) != 1 {
					t.Fatalf("discovery: %+v %v", d, err)
				}
				switch change {
				case "load symlink":
					swap(false)
				case "load parent symlink":
					swap(true)
				case "load deny":
					req.Deny.patterns = []string{normalize(root) + "/**"}
				case "load growth":
					f, err := os.OpenFile(filepath.Join(root, filepath.FromSlash(rel)), os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					err = f.Truncate(req.Source.MaxFileBytes + 1)
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				p, err := d.Candidates[0].Load(context.Background())
				if err == nil || len(p.Bytes) != 0 {
					t.Fatalf("unsafe load returned bytes: %s %v", p.Bytes, err)
				}
			} else if len(d.Candidates) != 0 {
				t.Fatalf("unsafe candidate: %+v %v", d, err)
			}
			if change == "oversize" && len(d.Oversize) != 1 {
				t.Fatalf("missing cap diagnostic: %+v", d)
			}
			if change == "cancelled" && err != context.Canceled {
				t.Fatalf("cancellation: %v", err)
			}
		})
	}
}

func BenchmarkOpenCodeReferencedSpills(b *testing.B) {
	root := b.TempDir()
	var refs []string
	for i := 0; i < 100; i++ {
		rel := "tool-output/tool_" + strings.Repeat("a", i+1)
		writeSpill(b, root, rel, strings.Repeat("full output\n", 750))
		refs = append(refs, filepath.Join(root, filepath.FromSlash(rel)))
	}
	spillStore(b, root, "opencode.db", "split", "/work/api", refs)
	req := spillRequest(b, root)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := (opencodeToolOutputs{}).Discover(req)
		if err != nil || len(d.Candidates) != 100 {
			b.Fatalf("discovery: %+v %v", d, err)
		}
		for _, c := range d.Candidates {
			if _, err := c.Load(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func TestOpenCodeJSONOnlyHistoryUnsupported(t *testing.T) {
	root := t.TempDir()
	for _, kind := range []string{"session", "message", "part"} {
		writeSpill(t, root, "storage/"+kind+"/project/stale.json", `{"id":"stale","text":"must not upload"}`)
	}
	req := spillRequest(t, root)
	d, err := (opencodeToolOutputs{}).Discover(req)
	if err != nil || len(d.Candidates) != 0 || d.Health != RootPresentNoMatch {
		t.Fatalf("JSON-only store: %+v %v", d, err)
	}
	d, err = (opencodeSessions{}).Discover(Request{Source: req.All[0]})
	if err != nil || len(d.Candidates) != 0 || d.Health != RootPresentNoMatch {
		t.Fatalf("JSON-only transcripts: %+v %v", d, err)
	}
	spillStore(t, root, "opencode.db", "v1", "/work/api", nil)
	d, err = (opencodeSessions{}).Discover(Request{Source: req.All[0]})
	if err != nil || len(d.Candidates) != 1 {
		t.Fatalf("migrated store: %+v %v", d, err)
	}
	p, err := d.Candidates[0].Load(context.Background())
	if err != nil || strings.Contains(string(p.Bytes), "stale") {
		t.Fatalf("stale JSON collected: %s %v", p.Bytes, err)
	}
}
