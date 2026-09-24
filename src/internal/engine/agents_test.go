package engine_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestAgentSessionPipeline(t *testing.T) {
	for _, family := range []string{"pi", "opencode", "hermes"} {
		t.Run(family, func(t *testing.T) {
			f := newFixture(t)
			root := filepath.Join(f.home, family)
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			catalog, err := sources.Load()
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := catalog.Source(family + "-sessions")
			var update func()
			if family == "pi" {
				path := filepath.Join(root, "test.jsonl")
				body := `{"type":"session","id":"custom-pi-session","cwd":"/work/test","version":3}` + "\n" + `{"type":"message","id":"1","message":{"role":"user","content":"test@example.com"}}` + "\n"
				if err := os.WriteFile(path, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				update = func() {
					if err := os.WriteFile(path, []byte(body+`{"type":"message","id":"2","message":{"role":"user","content":"next"}}`+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				name := "opencode.db"
				if family == "hermes" {
					name = "state.db"
				}
				db, err := sql.Open("sqlite", filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				schema := `PRAGMA journal_mode=WAL;
 CREATE TABLE session(id TEXT,parent_id TEXT,directory TEXT,title TEXT,version TEXT,time_created INTEGER,time_updated INTEGER);
 CREATE TABLE message(id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 CREATE TABLE part(id TEXT,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 INSERT INTO session VALUES ('ses_f2a37fe70ffe5gsGOoFrPMZOtR',NULL,'/work/test','test','1.18.20',1000,1000);
 INSERT INTO message VALUES ('m','ses_f2a37fe70ffe5gsGOoFrPMZOtR',1000,1000,'{"role":"user","text":"test@example.com"}');`
				updateSQL := `UPDATE message SET data='{"role":"user","text":"next"}'`
				if family == "hermes" {
					schema = `PRAGMA journal_mode=WAL;
 CREATE TABLE sessions(id TEXT,parent_session_id TEXT,source TEXT,model TEXT,cwd TEXT,started_at REAL,ended_at REAL,title TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER);
 CREATE TABLE messages(id INTEGER,session_id TEXT,role TEXT,content TEXT,tool_call_id TEXT,tool_calls TEXT,tool_name TEXT,timestamp REAL,finish_reason TEXT);
 INSERT INTO sessions VALUES ('date_non_uuid',NULL,'cli','model','/work/test',1,NULL,'test',5,2,0,0);
 INSERT INTO messages VALUES (1,'date_non_uuid','user','test@example.com',NULL,NULL,NULL,1,NULL);`
					updateSQL = `UPDATE messages SET content='next'`
				}
				if _, err = db.Exec(schema); err != nil {
					t.Fatal(err)
				}
				update = func() {
					if _, err := db.Exec(updateSQL); err != nil {
						t.Fatal(err)
					}
				}
			}
			o := f.opts()
			o.Plan.Sources = []sources.Resolved{{Source: spec, Root: root, Enabled: true, SpecFingerprint: sources.SpecFingerprint(spec)}}
			now := o.Now()
			o.Now = func() time.Time { return now }
			run := func() engine.Report {
				t.Helper()
				rep, err := engine.Run(context.Background(), f.store, o)
				if err != nil {
					t.Fatal(err)
				}
				return rep
			}
			first := run()
			if first.Shipped != 1 {
				t.Fatalf("first run: %+v", first)
			}
			key := f.port.keys()[0]
			obj, _ := f.port.get(key)
			manifest, raw, err := transforms.Open(obj.Body, f.unit.Identity)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.SourceID != spec.ID || manifest.Redaction == nil {
				t.Fatalf("missing source/redaction: %+v", manifest)
			}
			if strings.Contains(string(raw), "test@example.com") || !strings.Contains(string(raw), "__REDACTED:email__") {
				t.Fatalf("payload not scrubbed: %s", raw)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid JSON: %s", line)
				}
				if family != "pi" {
					var row map[string]any
					if err := json.Unmarshal([]byte(line), &row); err != nil {
						t.Fatal(err)
					}
					for _, key := range []string{"session_key", "event_key"} {
						value, ok := row[key].(string)
						if !ok || len(value) != 36 || strings.Contains(value, "REDACTED") {
							t.Fatalf("join key %s corrupted: %s", key, line)
						}
					}
				}
			}
			now = now.Add(time.Minute)
			second := run()
			if second.Shipped != 0 || second.Unchanged != 1 {
				t.Fatalf("repeat run: %+v", second)
			}
			update()
			now = now.Add(time.Minute)
			third := run()
			if third.Shipped != 1 {
				t.Fatalf("changed session: %+v", third)
			}
			if got := f.port.keys(); len(got) != 1 || got[0] != key {
				t.Fatalf("session update changed logical key: %v", got)
			}
		})
	}
}

// Opt-in: only explicit isolated smoke-test stores, never a developer's default history.
func TestLiveAgentSessionStores(t *testing.T) {
	base := os.Getenv("SHIPPER_AGENT_TEST_STORES")
	if base == "" {
		t.Skip("set SHIPPER_AGENT_TEST_STORES to isolated test stores")
	}
	out := os.Getenv("SHIPPER_AGENT_TEST_EXPORT")
	for _, item := range []struct{ family, dir string }{{"pi", "pi/sessions"}, {"opencode", "opencode/data/opencode"}, {"hermes", "hermes"}} {
		t.Run(item.family, func(t *testing.T) {
			f := newFixture(t)
			catalog, err := sources.Load()
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := catalog.Source(item.family + "-sessions")
			o := f.opts()
			o.Plan.Sources = []sources.Resolved{{Source: spec, Root: filepath.Join(base, item.dir), Enabled: true, SpecFingerprint: sources.SpecFingerprint(spec)}}
			rep, err := engine.Run(context.Background(), f.store, o)
			if err != nil {
				t.Fatal(err)
			}
			if rep.Shipped == 0 || rep.Failed != 0 {
				t.Fatalf("live collection: %+v", rep)
			}
			t.Logf("%s: %d sessions shipped", item.family, rep.Shipped)
			for i, key := range f.port.keys() {
				obj, _ := f.port.get(key)
				manifest, raw, err := transforms.Open(obj.Body, f.unit.Identity)
				if err != nil {
					t.Fatal(err)
				}
				if out != "" {
					dir := filepath.Join(out, item.family)
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
					name := filepath.Join(dir, fmt.Sprintf("%03d", i))
					if err := os.WriteFile(name+".jsonl", raw, 0600); err != nil {
						t.Fatal(err)
					}
					b, _ := json.Marshal(manifest)
					if err := os.WriteFile(name+".manifest.json", b, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
