package sqliteread_test

import (
	"context"
	"database/sql"
	"encoding/json"
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
	path := filepath.Join(t.TempDir(), name)
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
	for _, family := range []string{"opencode", "hermes"} {
		t.Run(family, func(t *testing.T) {
			path, db := sessionStore(t, family)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := sqliteread.ListSessions(context.Background(), path, family)
			if err != nil {
				t.Fatal(err)
			}
			if len(sessions) == 0 || sessions[0].CWD == "" {
				t.Fatalf("no routing metadata: %+v", sessions)
			}
			data, err := sqliteread.ReadSession(context.Background(), path, family, sessions[0].ID, 1<<20)
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
			if got, err := sqliteread.ReadSession(context.Background(), path, family, sessions[0].ID, 10); err == nil || got != nil {
				t.Fatal("oversize snapshot must fail without partial bytes")
			}
			again, err := sqliteread.ReadSession(context.Background(), path, family, sessions[0].ID, 1<<20)
			if err != nil || string(again) != string(data) {
				t.Fatal("unchanged snapshots differ")
			}
			if family == "opencode" {
				_, err = db.Exec(`UPDATE part SET data='{"type":"text","text":"updated"}' WHERE id='p1'`)
			} else {
				_, err = db.Exec(`UPDATE messages SET content='updated' WHERE id=1`)
			}
			if err != nil {
				t.Fatal(err)
			}
			changed, err := sqliteread.ReadSession(context.Background(), path, family, sessions[0].ID, 1<<20)
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
			if _, err := sqliteread.ReadSession(ctx, path, family, sessions[0].ID, 1<<20); err == nil {
				t.Fatal("cancelled read succeeded")
			}
		})
	}
}

func TestSessionReadMissingMalformedAndOldUsage(t *testing.T) {
	path, db := sessionStore(t, "opencode")
	if raw, err := sqliteread.ReadSession(context.Background(), path, "opencode", "missing", 1<<20); err == nil || raw != nil {
		t.Fatal("missing session succeeded")
	}
	if _, err := db.Exec(`UPDATE message SET data='invalid' WHERE id='m1'`); err != nil {
		t.Fatal(err)
	}
	if raw, err := sqliteread.ReadSession(context.Background(), path, "opencode", "ses_../weird", 1<<20); err == nil || raw != nil {
		t.Fatal("malformed JSON shipped")
	}
	path, db = sessionStore(t, "hermes")
	if _, err := db.Exec(`DROP TABLE session_model_usage`); err != nil {
		t.Fatal(err)
	}
	raw, err := sqliteread.ReadSession(context.Background(), path, "hermes", "20260925_a", 1<<20)
	if err != nil || !strings.Contains(string(raw), `"input_tokens":10`) {
		t.Fatalf("legacy session totals lost: %s %v", raw, err)
	}
	if _, err := sqliteread.ListSessions(context.Background(), path, "unknown"); err == nil {
		t.Fatal("unknown family accepted")
	}
}
