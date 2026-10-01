package sources

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

func TestSessionDatabaseDiscoveryScope(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"state.db", "profiles/work/state.db", "cache/state.db", "lazy-packages/a/state.db", "profiles/work/auth.json"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "state.db"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "profiles", "link")); err != nil {
		t.Fatal(err)
	}
	src := Resolved{Source: Source{Family: "hermes", Include: []string{"state.db", "profiles/*/state.db"}}, Root: root}
	got, bad, err := sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 2 {
		t.Fatalf("discovery: %v %+v %v", got, bad, err)
	}
	src.Exclude = []string{"profiles/**"}
	got, bad, err = sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 1 || got[0].RelPath != "state.db" {
		t.Fatalf("exclude: %v %+v %v", got, bad, err)
	}
	if err := os.Remove(filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "state.db"), filepath.Join(root, "state.db")); err != nil {
		t.Fatal(err)
	}
	got, bad, err = sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 0 {
		t.Fatalf("symlink escaped scope: %v %+v %v", got, bad, err)
	}
}

func TestAgentSessionRepositoryMarkers(t *testing.T) {
	catalog, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	project := repoDir(t, home, "work with spaces", true)
	filter := catalog.RepoFilter()
	for _, id := range []string{"pi-sessions", "opencode-sessions", "hermes-sessions"} {
		spec, ok := catalog.Source(id)
		if !ok {
			t.Fatal(id)
		}
		src := Resolved{Source: spec, Root: home}
		cand := Candidate{Path: filepath.Join(home, "virtual.jsonl"), CWD: project}
		if !filter.Match(src, cand) {
			t.Errorf("%s ignored repository was collected", id)
		}
	}
}

func TestSessionDatabasePatternsAndDeniedRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "custom.db"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	src := Resolved{Source: Source{Include: []string{"custom.db", "*.db"}}, Root: root}
	got, bad, err := sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 1 || got[0].RelPath != "custom.db" {
		t.Fatalf("YAML patterns/dedup: %v %+v %v", got, bad, err)
	}
	deny := &List{patterns: []string{normalize(root) + "/**"}}
	got, bad, err = sessionDatabases(src, deny)
	if err != nil || len(got) != 0 || bad.count != 1 || !strings.Contains(bad.reason(), "deny list refuses") {
		t.Fatalf("denied root lost diagnostic: %v %+v %v", got, bad, err)
	}
}

func TestSessionDatabaseLiteralSymlinkPrefix(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "work"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "work", "state.db"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "profiles")); err != nil {
		t.Fatal(err)
	}
	src := Resolved{Source: Source{Include: []string{"profiles/*/state.db"}}, Root: root}
	got, bad, err := sessionDatabases(src, nil)
	if err != nil || bad.count != 0 || len(got) != 0 {
		t.Fatalf("literal-prefix symlink escaped scope: %v %+v %v", got, bad, err)
	}
}

func writeSessionStore(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE sessions(id TEXT,parent_session_id TEXT,source TEXT,model TEXT,cwd TEXT,started_at REAL,ended_at REAL,title TEXT,input_tokens INTEGER,output_tokens INTEGER,cache_read_tokens INTEGER,cache_write_tokens INTEGER);
CREATE TABLE messages(id INTEGER,session_id TEXT,role TEXT,content TEXT,tool_call_id TEXT,tool_calls TEXT,tool_name TEXT,timestamp REAL,finish_reason TEXT);
INSERT INTO sessions VALUES ('same',NULL,'cli','model','/work/api',1,NULL,'test',5,2,0,0);`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO messages VALUES (1,'same','user',?,NULL,NULL,NULL,1,NULL)`, body); err != nil {
		t.Fatal(err)
	}
}

func TestSessionSnapshotReservesItsReadCap(t *testing.T) {
	for _, cap := range []int64{0, 4096, 64 << 20} {
		root := t.TempDir()
		writeSessionStore(t, root, "inside")
		src := Resolved{Source: Source{ID: "hermes-sessions", Include: []string{"state.db"}, MaxFileBytes: cap}, Root: root}
		d, err := discoverSessions(Request{Source: src}, sqliteread.HermesSessions{})
		if err != nil || len(d.Candidates) != 1 {
			t.Fatalf("discovery: %+v %v", d, err)
		}
		want := cap
		if want == 0 {
			want = sqliteread.DefaultSessionMaxBytes
		}
		c := d.Candidates[0]
		payload, err := c.Load(context.Background())
		if err != nil || len(payload.Bytes) == 0 {
			t.Fatalf("load: %v", err)
		}
		if c.Size != want {
			t.Fatalf("cap %d: admission charge %d, want %d", cap, c.Size, want)
		}
	}
}

func TestSessionLoadRechecksProfilePath(t *testing.T) {
	for _, change := range []string{"unchanged", "profile symlink", "database symlink", "denied after discovery", "root symlink retargeted"} {
		t.Run(change, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			profile := filepath.Join(root, "profiles", "work")
			writeSessionStore(t, profile, "INSIDE-CONTENT")
			writeSessionStore(t, outside, "OUTSIDE-CONTENT")
			namedRoot := root
			if change == "root symlink retargeted" {
				namedRoot = filepath.Join(t.TempDir(), "root")
				if err := os.Symlink(root, namedRoot); err != nil {
					t.Fatal(err)
				}
			}
			src := Resolved{Source: Source{ID: "hermes-sessions", Include: []string{"profiles/*/state.db"}, MaxFileBytes: 64 << 20}, Root: namedRoot}
			deny := &List{}
			d, err := discoverSessions(Request{Source: src, Deny: deny}, sqliteread.HermesSessions{})
			if err != nil || len(d.Candidates) != 1 {
				t.Fatalf("discovery: %+v %v", d, err)
			}
			switch change {
			case "profile symlink", "database symlink":
				from, to := profile, outside
				if change == "database symlink" {
					from, to = filepath.Join(from, "state.db"), filepath.Join(to, "state.db")
				}
				if err := os.Rename(from, from+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(to, from); err != nil {
					t.Fatal(err)
				}
			case "denied after discovery":
				resolved, err := filepath.EvalSymlinks(profile)
				if err != nil {
					t.Fatal(err)
				}
				deny.patterns = []string{normalize(resolved) + "/**"}
			case "root symlink retargeted":
				writeSessionStore(t, filepath.Join(outside, "profiles", "work"), "OUTSIDE-CONTENT")
				if err := os.Remove(namedRoot); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, namedRoot); err != nil {
					t.Fatal(err)
				}
			}
			payload, err := d.Candidates[0].Load(context.Background())
			if change == "unchanged" || change == "root symlink retargeted" {
				if err != nil || !strings.Contains(string(payload.Bytes), "INSIDE-CONTENT") || strings.Contains(string(payload.Bytes), "OUTSIDE-CONTENT") {
					t.Fatalf("must read the original resolved root: %s %v", payload.Bytes, err)
				}
			} else if err == nil || len(payload.Bytes) != 0 {
				t.Fatalf("changed path must return an error without bytes: %s %v", payload.Bytes, err)
			}
		})
	}
}

func TestHermesProfileKeysSurviveRootMove(t *testing.T) {
	baseline := map[string]string{}
	for copy := 0; copy < 2; copy++ {
		root := t.TempDir()
		for _, dir := range []string{".", "profiles/work", "profiles/personal"} {
			writeSessionStore(t, filepath.Join(root, filepath.FromSlash(dir)), "identical session")
		}
		src := Resolved{Source: Source{ID: "hermes-sessions", Include: []string{"state.db", "profiles/*/state.db"}}, Root: root}
		d, err := discoverSessions(Request{Source: src}, sqliteread.HermesSessions{})
		if err != nil || len(d.Candidates) != 3 {
			t.Fatalf("discovery: %+v %v", d, err)
		}
		keys := map[string]string{}
		for _, candidate := range d.Candidates {
			payload, err := candidate.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if copy == 0 {
				baseline[candidate.RelPath] = string(payload.Bytes)
			} else if baseline[candidate.RelPath] != string(payload.Bytes) {
				t.Fatalf("moving the root changed snapshot: %s", candidate.RelPath)
			}
			var row map[string]any
			if err := json.Unmarshal(bytes.SplitN(payload.Bytes, []byte("\n"), 2)[0], &row); err != nil {
				t.Fatal(err)
			}
			key := row["session_key"].(string)
			if previous, exists := keys[key]; exists {
				t.Fatalf("profiles share session key: %s and %s", previous, candidate.RelPath)
			}
			keys[key] = candidate.RelPath
		}
	}
}
