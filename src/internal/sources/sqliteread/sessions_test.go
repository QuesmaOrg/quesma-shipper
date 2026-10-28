package sqliteread_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

func sessionStore(t *testing.T, family string) (string, *sql.DB) {
	t.Helper()
	name := "sessions ?# ü.db"
	if runtime.GOOS == "windows" {
		name = "sessions # ü.db"
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schema := `PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
 CREATE TABLE credentials(secret TEXT); INSERT INTO credentials VALUES ('NEVER-SHIP-AUTH');`
	if family == "opencode" {
		schema += `
 CREATE TABLE session(id TEXT PRIMARY KEY,parent_id TEXT,directory TEXT,title TEXT,version TEXT,time_created INTEGER,time_updated INTEGER,secret TEXT);
 CREATE TABLE message(id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 CREATE TABLE part(id TEXT,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 INSERT INTO session VALUES ('ses_../weird','parent','/work/with spaces','test','1.18.20',1000,1000,'NEVER-SHIP-EXTRA-COLUMN');
 INSERT INTO session VALUES ('other',NULL,'/work/other','other','1.18.20',2000,2000,NULL);
 INSERT INTO message VALUES ('m1','ses_../weird',1000,1000,'{"role":"user"}');
 INSERT INTO message VALUES ('m2','other',2000,2000,'{"role":"user","text":"OTHER-SESSION"}');
 INSERT INTO part VALUES ('p1','m1','ses_../weird',1000,1000,'{"type":"text","text":"hello"}');`
	} else if family == "opencode-v2" {
		schema += `
 CREATE TABLE session_v2(id TEXT PRIMARY KEY,project_id TEXT,parent_id TEXT,fork_session_id TEXT,fork_boundary TEXT,directory TEXT,path TEXT,title TEXT,version TEXT,permission TEXT,time_created INTEGER,time_updated INTEGER,cost REAL,tokens_input INTEGER,tokens_output INTEGER,tokens_reasoning INTEGER,tokens_cache_read INTEGER,tokens_cache_write INTEGER);
 CREATE TABLE session_message(id TEXT PRIMARY KEY,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,time_updated INTEGER,data TEXT);
 INSERT INTO session_v2 VALUES ('ses_../weird','prj','parent','ses_fork','{"seq":3}','/work/with spaces','sub',NULL,'2.0.22','NEVER-SHIP-PERMISSION',1000,1000,0.75,100,20,5,300,4);
 INSERT INTO session_v2 VALUES ('other','prj',NULL,NULL,NULL,'/work/other','','other','2.0.22',NULL,2000,2000,0,0,0,0,0,0);
 INSERT INTO session_message VALUES ('msg_b','ses_../weird','assistant',2,1000,1000,'{"content":[{"type":"text","text":"hello"}]}');
 INSERT INTO session_message VALUES ('msg_a','ses_../weird','user',1,1000,1000,'{"text":"hi"}');
 INSERT INTO session_message VALUES ('msg_c','other','user',1,2000,2000,'{"text":"OTHER-SESSION"}');`
	} else {
		schema += `
 CREATE TABLE sessions(id TEXT PRIMARY KEY,parent_session_id TEXT,source TEXT,model TEXT,cwd TEXT,started_at REAL,ended_at REAL,title TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER,model_config TEXT);
 CREATE TABLE messages(id INTEGER,session_id TEXT,role TEXT,content TEXT,tool_call_id TEXT,tool_calls TEXT,tool_name TEXT,timestamp REAL,finish_reason TEXT);
 CREATE TABLE session_model_usage(session_id TEXT,model TEXT,billing_provider TEXT,billing_base_url TEXT,billing_mode TEXT,task TEXT,api_call_count INTEGER,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER,reasoning_tokens INTEGER,estimated_cost_usd REAL,actual_cost_usd REAL,cost_status TEXT,cost_source TEXT,first_seen REAL,last_seen REAL);
 INSERT INTO sessions VALUES ('20260925_a',NULL,'cli','model-a','/work/hermes',1,NULL,'hello',10,2,0,0,'NEVER-SHIP-CONFIG');
 INSERT INTO messages VALUES (1,'20260925_a','user','hello',NULL,NULL,NULL,1,NULL);
 INSERT INTO session_model_usage VALUES ('20260925_a','model-a','custom','https://user:secret@example.test/v1','api','',1,10,2,0,0,0,0.01,0,'estimated','test',1,2);
 INSERT INTO session_model_usage VALUES ('20260925_a','model-b','custom','http://localhost/v1','api','title_generation',1,5,1,0,0,0,0,0,'estimated','test',1,2);`
	}
	if _, err = db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return path, db
}

func TestSessionReadWALScopeAndBounds(t *testing.T) {
	for _, family := range []string{"opencode", "opencode-v2", "hermes"} {
		t.Run(family, func(t *testing.T) {
			path, db := sessionStore(t, family)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := reader(family).List(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) == 0 || sessions[0].CWD == "" {
				t.Fatalf("no routing metadata: %+v", sessions)
			}
			data, err := reader(family).Read(context.Background(), path, "state.db", sessions[0].ID, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"NEVER-SHIP", "OTHER-SESSION", "user:secret"} {
				if strings.Contains(string(data), bad) {
					t.Errorf("scope leak: %s", bad)
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid JSON: %s", line)
				}
			}
			if got, err := reader(family).Read(context.Background(), path, "state.db", sessions[0].ID, 10); err == nil || got != nil {
				t.Fatal("oversize snapshot must fail without partial bytes")
			}
			again, err := reader(family).Read(context.Background(), path, "state.db", sessions[0].ID, 1<<20)
			if err != nil || string(again) != string(data) {
				t.Fatal("unchanged snapshots differ")
			}
			switch family {
			case "opencode":
				_, err = db.Exec(`UPDATE part SET data='{"type":"text","text":"updated"}' WHERE id='p1'`)
			case "opencode-v2":
				_, err = db.Exec(`UPDATE session_message SET data='{"text":"updated"}' WHERE id='msg_a'`)
			default:
				_, err = db.Exec(`UPDATE messages SET content='updated' WHERE id=1`)
			}
			if err != nil {
				t.Fatal(err)
			}
			changed, err := reader(family).Read(context.Background(), path, "state.db", sessions[0].ID, 1<<20)
			if err != nil || !strings.Contains(string(changed), "updated") {
				t.Fatalf("WAL update missed: %s %v", changed, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("reader changed the main database")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := reader(family).Read(ctx, path, "state.db", sessions[0].ID, 1<<20); err == nil {
				t.Fatal("cancelled read succeeded")
			}
		})
	}
}

func TestSessionReadMissingMalformedAndOldUsage(t *testing.T) {
	path, db := sessionStore(t, "opencode")
	if raw, err := reader("opencode").Read(context.Background(), path, "state.db", "missing", 1<<20); err == nil || raw != nil {
		t.Fatal("missing session succeeded")
	}
	if _, err := db.Exec(`UPDATE message SET data='invalid' WHERE id='m1'`); err != nil {
		t.Fatal(err)
	}
	if raw, err := reader("opencode").Read(context.Background(), path, "state.db", "ses_../weird", 1<<20); err == nil || raw != nil {
		t.Fatal("malformed JSON shipped")
	}
	path, db = sessionStore(t, "hermes")
	if _, err := db.Exec(`DROP TABLE session_model_usage`); err != nil {
		t.Fatal(err)
	}
	raw, err := reader("hermes").Read(context.Background(), path, "state.db", "20260925_a", 1<<20)
	if err != nil || !strings.Contains(string(raw), `"input_tokens":10`) {
		t.Fatalf("legacy session totals lost: %s %v", raw, err)
	}
}

type sessionReader interface {
	List(context.Context, string) ([]sqliteread.Session, error)
	Read(context.Context, string, string, string, int64) ([]byte, error)
}

func reader(family string) sessionReader {
	if strings.HasPrefix(family, "opencode") {
		return sqliteread.OpenCodeSessions{}
	}
	return sqliteread.HermesSessions{}
}

func TestHermesProfileRecordKeys(t *testing.T) {
	path, _ := sessionStore(t, "hermes")
	seen := map[string]string{}
	for _, rel := range []string{"state.db", "profiles/work/state.db", "profiles/personal/state.db"} {
		raw, err := reader("hermes").Read(context.Background(), path, filepath.FromSlash(rel), "20260925_a", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		again, err := reader("hermes").Read(context.Background(), path, filepath.FromSlash(rel), "20260925_a", 1<<20)
		if err != nil || string(again) != string(raw) {
			t.Fatalf("unstable profile snapshot: %s: %v", rel, err)
		}
		var sessionKey string
		counts := map[string]int{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatal(err)
			}
			if row["session_id"] != "20260925_a" {
				t.Fatalf("raw session ID changed: %v", row)
			}
			key := row["session_key"].(string)
			if sessionKey == "" {
				sessionKey = key
			}
			if key != sessionKey {
				t.Fatalf("records disagree on session key: %s", rel)
			}
			for _, field := range []string{"session_key", "event_key", "usage_id"} {
				if key, ok := row[field].(string); ok {
					counts[field]++
					if previous, exists := seen[field+key]; exists && previous != rel {
						t.Fatalf("%s collision between %s and %s", field, previous, rel)
					}
					seen[field+key] = rel
				}
			}
		}
		if rel == "state.db" && sessionKey != "ae8c1970-e8d6-460c-3d7d-20e61e4134e8" {
			t.Fatalf("default-profile key changed: %s", sessionKey)
		}
		if counts["session_key"] != 4 || counts["event_key"] != 2 || counts["usage_id"] != 2 {
			t.Fatalf("missing record keys: %v", counts)
		}
	}
}

func TestOpenCodeChannelRecordKeys(t *testing.T) {
	path, _ := sessionStore(t, "opencode")
	keys := map[string]string{}
	for _, rel := range []string{"opencode.db", "opencode-stable.db", "opencode-dev.db"} {
		raw, err := reader("opencode").Read(context.Background(), path, rel, "ses_../weird", 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(strings.SplitN(string(raw), "\n", 2)[0]), &row); err != nil {
			t.Fatal(err)
		}
		key, _ := row["session_key"].(string)
		if previous, exists := keys[key]; exists {
			t.Fatalf("%s and %s share session key %s", previous, rel, key)
		}
		keys[key] = rel
		if rel == "opencode.db" && key != "e4b7a824-333d-ecfe-dc1c-3e4ae6ac1e80" {
			t.Fatalf("default database key changed: %s", key)
		}
	}
}

func TestOpenCodeNextDatabase(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "opencode-next.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE session(id TEXT,parent_id TEXT,directory TEXT,title TEXT,version TEXT,time_created INTEGER,time_updated INTEGER);
 CREATE TABLE message(id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 CREATE TABLE part(id TEXT,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 INSERT INTO session VALUES ('ses_next',NULL,'/work/next','next','1.2.27',1000,1000);`); err != nil {
		t.Fatal(err)
	}
	// Without opencode.db, or before OpenCode imports it, opencode-next.db is the only copy of its history.
	if sessions, err := reader("opencode").List(context.Background(), path); err != nil || len(sessions) != 1 {
		t.Fatalf("only database: %v %v", sessions, err)
	}
	main, err := sql.Open("sqlite", filepath.Join(root, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer main.Close()
	if _, err := main.Exec(`CREATE TABLE session_v2(id TEXT); INSERT INTO session_v2 VALUES ('ses_other')`); err != nil {
		t.Fatal(err)
	}
	if sessions, err := reader("opencode").List(context.Background(), path); err != nil || len(sessions) != 1 {
		t.Fatalf("not imported: %v %v", sessions, err)
	}
	if _, err := main.Exec(`INSERT INTO session_v2 VALUES ('ses_next')`); err != nil {
		t.Fatal(err)
	}
	if sessions, err := reader("opencode").List(context.Background(), path); err != nil || len(sessions) != 0 {
		t.Fatalf("imported: %v %v", sessions, err)
	}
}

func TestOpenCodeLayouts(t *testing.T) {
	ctx := context.Background()
	records := func(path, rel, id string) []map[string]any {
		t.Helper()
		raw, err := reader("opencode").Read(ctx, path, rel, id, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatal(err)
			}
			out = append(out, row)
		}
		return out
	}
	// Split V2: messages follow seq, not the tied timestamps or IDs.
	path, db := sessionStore(t, "opencode-v2")
	rows := records(path, "opencode.db", "ses_../weird")
	var got []string
	for _, row := range rows {
		got = append(got, fmt.Sprint(row["type"], "/", row["message_type"], "/", row["seq"]))
	}
	if strings.Join(got, " ") != "session/<nil>/<nil> session_message/user/1 session_message/assistant/2" {
		t.Fatalf("V2 records: %v", got)
	}
	if rows[0]["title"] != nil || rows[0]["fork_session_id"] != "ses_fork" || rows[0]["cwd"] != "/work/with spaces" {
		t.Fatalf("V2 session metadata: %v", rows[0])
	}
	if fmt.Sprint(rows[0]["cost"], rows[0]["tokens_input"], rows[0]["tokens_output"], rows[0]["tokens_reasoning"], rows[0]["tokens_cache_read"], rows[0]["tokens_cache_write"]) != "0.75 100 20 5 300 4" {
		t.Fatalf("V2 session usage totals: %v", rows[0])
	}
	if rows[0]["session_key"] != "e4b7a824-333d-ecfe-dc1c-3e4ae6ac1e80" {
		t.Fatalf("default database key changed: %v", rows[0]["session_key"])
	}

	// Migrated store: upstream copies 1.x sessions into the 2.x tables under the same IDs and keeps the originals.
	if _, err := db.Exec(`CREATE TABLE session(id TEXT,parent_id TEXT,directory TEXT,title TEXT,version TEXT,time_created INTEGER,time_updated INTEGER);
 CREATE TABLE message(id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 CREATE TABLE part(id TEXT,message_id TEXT,session_id TEXT,time_created INTEGER,time_updated INTEGER,data TEXT);
 INSERT INTO session VALUES ('ses_../weird',NULL,'/work/before move','test','1.2.27',1000,1000);
 INSERT INTO message VALUES ('msg_a','ses_../weird',1000,1000,'{"role":"user"}');
 INSERT INTO session VALUES ('legacy',NULL,'/work/legacy','old','1.2.27',500,500);
 INSERT INTO message VALUES ('m1','legacy',500,500,'{"role":"user"}');
 INSERT INTO session VALUES ('moved',NULL,'/work/allowed','empty','1.2.27',3000,3000);
 INSERT INTO session_v2 VALUES ('moved','prj',NULL,NULL,NULL,'/work/marked','',NULL,'2.0.22',NULL,3000,3000,0,0,0,0,0,0);
 INSERT INTO session_message VALUES ('msg_m','moved','user',1,3000,3000,'{"text":"continued in 2.x"}');
 INSERT INTO session VALUES ('reverted',NULL,'/work/reverted','r','1.2.27',4000,4000);
 INSERT INTO message VALUES ('m_r','reverted',4000,4000,'{"role":"user","text":"REVERTED-LEGACY"}');
 INSERT INTO session_v2 VALUES ('reverted','prj',NULL,NULL,NULL,'/work/reverted','',NULL,'2.0.22',NULL,4000,4000,0,0,0,0,0,0);`); err != nil {
		t.Fatal(err)
	}
	sessions, err := reader("opencode").List(ctx, path)
	var listed []string
	for _, s := range sessions {
		listed = append(listed, s.ID)
		// Repository markers are checked against the listed directory, so it must be the exported one.
		if rows := records(path, "opencode.db", s.ID); rows[0]["cwd"] != s.CWD {
			t.Fatalf("%s listed in %s but exported from %v", s.ID, s.CWD, rows[0]["cwd"])
		}
	}
	if err != nil || strings.Join(listed, ",") != "legacy,ses_../weird,other,moved,reverted" {
		t.Fatalf("migrated listing: %v %v", listed, err)
	}
	// An empty 1.x session continued in 2.x ships its 2.x copy.
	if rows := records(path, "opencode.db", "moved"); len(rows) != 2 || rows[0]["cwd"] != "/work/marked" || rows[1]["type"] != "session_message" {
		t.Fatalf("migrated empty session: %v", rows)
	}
	got = nil
	for _, row := range records(path, "opencode.db", "ses_../weird") {
		got = append(got, fmt.Sprint(row["type"], "/", row["version"]))
	}
	if strings.Join(got, " ") != "session/2.0.22 session_message/<nil> session_message/<nil>" {
		t.Fatalf("migrated session must ship once, from the 2.x copy: %v", got)
	}
	// Reverting to the first message deletes the 2.x messages; the stale 1.x ones must not come back.
	if rows := records(path, "opencode.db", "reverted"); len(rows) != 1 || rows[0]["version"] != "2.0.22" {
		t.Fatalf("reverted 2.x session: %v", rows)
	}
	if rows := records(path, "opencode.db", "other"); len(rows) != 2 || rows[0]["version"] != "2.0.22" || rows[1]["type"] != "session_message" {
		t.Fatalf("2.x-only session in migrated store: %v", rows)
	}

	// A partial layout is a clear schema error, not a raw missing-table error or a silent omission.
	for _, drop := range []string{"part", "session_message"} {
		if _, err := db.Exec(`DROP TABLE ` + drop); err != nil {
			t.Fatal(err)
		}
		if _, err := reader("opencode").List(ctx, path); err == nil || !strings.Contains(err.Error(), "unsupported OpenCode database schema") {
			t.Fatalf("without %s: %v", drop, err)
		}
	}

	// Shared V2 layout: a session with only native messages ships them; one with legacy messages keeps its legacy records.
	path, db = sessionStore(t, "opencode")
	if _, err := db.Exec(`CREATE TABLE session_message(id TEXT,session_id TEXT,type TEXT,seq INTEGER,time_created INTEGER,time_updated INTEGER,data TEXT);
 INSERT INTO session VALUES ('fresh',NULL,'/work/fresh','fresh','2.0.0',3000,3000,NULL);
 INSERT INTO session_message VALUES ('msg_n','fresh','user',1,3000,3000,'{"text":"native"}');
 INSERT INTO session_message VALUES ('msg_c','ses_../weird','user',1,1000,1000,'{"text":"copy"}');`); err != nil {
		t.Fatal(err)
	}
	if rows := records(path, "opencode.db", "fresh"); len(rows) != 2 || rows[1]["type"] != "session_message" {
		t.Fatalf("shared layout: %v", rows)
	}
	if rows := records(path, "opencode.db", "ses_../weird"); len(rows) != 3 || rows[1]["type"] != "message" {
		t.Fatalf("shared layout legacy session: %v", rows)
	}
}

func TestOpenCodeV2ExecutionKeys(t *testing.T) {
	path, db := sessionStore(t, "opencode-v2")
	if _, err := db.Exec(`INSERT INTO session_v2 VALUES ('twins','prj',NULL,NULL,NULL,'/work','',NULL,'2.0.22',NULL,3000,3000,0,0,0,0,0,0);
 INSERT INTO session_message VALUES ('msg_s','twins','system',1,3000,3000,'{"text":"same"}');
 INSERT INTO session_message VALUES ('msg_y','twins','synthetic',2,3000,3000,'{"text":"same"}');`); err != nil {
		t.Fatal(err)
	}
	raw, err := reader("opencode").Read(context.Background(), path, "opencode.db", "twins", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[any]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n")[1:] {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		keys[row["execution_key"]] = true
	}
	if len(keys) != 2 {
		t.Fatalf("system and synthetic records with the same payload share an execution key: %s", raw)
	}
}
