package sqliteread

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func boundTestStore(t *testing.T, dir, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", sessionFileURI(filepath.ToSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("CREATE TABLE payload(content TEXT); INSERT INTO payload VALUES (?)", content); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSessionOpenRejectsReplacementAfterAnchoring(t *testing.T) {
	for _, replacement := range []string{"profile symlink", "profile directory", "database file"} {
		t.Run(replacement, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			profile := filepath.Join(root, "profile")
			path := boundTestStore(t, profile, "inside")
			outside := filepath.Join(root, "outside")
			other := boundTestStore(t, outside, "outside")
			bound, err := anchorSessionPath(path)
			if err != nil {
				t.Fatal(err)
			}
			from, to := profile, outside
			if replacement == "database file" {
				from, to = path, other
			}
			if err := os.Rename(from, from+"-old"); err != nil {
				bound.parent.Close()
				t.Fatal(err)
			}
			if replacement == "profile symlink" {
				err = os.Symlink(to, from)
			} else {
				err = os.Rename(to, from)
			}
			if err != nil {
				bound.parent.Close()
				t.Fatal(err)
			}
			conn, err := bound.open(context.Background())
			if err == nil {
				conn.Close()
				t.Fatal("opened replacement after anchoring the original file")
			}
		})
	}
}

func TestSessionQueryPreservesValuesAndInterrupts(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := boundTestStore(t, root, "test")
	conn, err := openSessions(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	text := "quote' and zero\x00 byte"
	err = conn.query(context.Background(), "SELECT ? AS text, 42 AS integer, 1.5 AS real, NULL AS empty", []string{text}, func(row map[string]any) error {
		if row["text"] != text || row["integer"] != int64(42) || row["real"] != 1.5 || row["empty"] != nil {
			t.Fatalf("values changed: %#v", row)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows := 0
	err = conn.query(ctx, "WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n) SELECT x FROM n", nil, func(map[string]any) error {
		rows++
		if rows == 10 {
			cancel()
		}
		return nil
	})
	if err != context.Canceled {
		t.Fatalf("canceled query returned %v", err)
	}
	if err := conn.query(context.Background(), "SELECT 1", nil, nil); err != nil {
		t.Fatalf("connection unusable after cancellation: %v", err)
	}
}

func TestSessionOpenRejectsSymlinkSidecars(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := boundTestStore(t, filepath.Join(root, "source"), "inside")
	writer, err := sql.Open("sqlite", sessionFileURI(filepath.ToSlash(source)))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("PRAGMA journal_mode=WAL; INSERT INTO payload VALUES ('new')"); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		name := suffix
		if name == "" {
			name = "regular sidecars"
		}
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(root, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state.db")
			// Copy the idle writer's fixture: Windows cannot rename its open sidecars.
			for _, ext := range []string{"", "-wal", "-shm"} {
				data, err := os.ReadFile(source + ext)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+ext, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if suffix != "" {
				side := path + suffix
				if err := os.Rename(side, side+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(side+"-moved", side); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := openSessions(context.Background(), path)
			if suffix != "" {
				if err == nil {
					conn.Close()
					t.Fatal("opened a symlink sidecar")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var contents []string
			if err := conn.query(context.Background(), "SELECT content FROM payload ORDER BY rowid", nil, func(row map[string]any) error {
				contents = append(contents, row["content"].(string))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if strings.Join(contents, ",") != "inside,new" {
				t.Fatalf("fixture lost committed WAL data: %v", contents)
			}
		})
	}
}

func TestSessionAnchorRejectsNestedLinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	boundTestStore(t, filepath.Join(root, "real"), "inside")
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "real"), alias); err != nil {
		t.Fatal(err)
	}
	if bound, err := anchorSessionPath(filepath.Join(alias, "state.db")); err == nil {
		bound.parent.Close()
		t.Fatal("followed nested symlink")
	} else if !strings.Contains(err.Error(), "not a directory") {
		t.Fatal(err)
	}
}

func TestSessionConnectionKeepsOneWALSnapshot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := boundTestStore(t, root, "before")
	writer, err := sql.Open("sqlite", sessionFileURI(filepath.ToSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("PRAGMA journal_mode=WAL; UPDATE payload SET content='snapshot'"); err != nil {
		t.Fatal(err)
	}
	conn, err := openSessions(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := writer.Exec("UPDATE payload SET content='later'; PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		t.Fatal(err)
	}
	check := func(c *sessionConnection, want string) {
		t.Helper()
		count := 0
		err := c.query(context.Background(), "SELECT content FROM payload", nil, func(row map[string]any) error {
			count++
			if row["content"] != want {
				t.Errorf("content=%v, want %s", row["content"], want)
			}
			return nil
		})
		if err != nil || count != 1 {
			t.Fatalf("read: %d rows, %v", count, err)
		}
	}
	check(conn, "snapshot")
	next, err := openSessions(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	check(next, "later")
	if err := conn.query(context.Background(), "UPDATE payload SET content='must not write'", nil, nil); err == nil {
		t.Fatal("read-only connection accepted a write")
	}
}
