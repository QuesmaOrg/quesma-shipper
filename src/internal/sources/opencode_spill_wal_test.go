package sources

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenCodeSpillsWALAndSessionIsolation(t *testing.T) {
	root := t.TempDir()
	spillStore(t, root, "opencode.db", "split", "/work/api", nil)
	writeSpill(t, root, "tool-output/tool_new", "committed full output")
	db, err := sql.Open("sqlite", filepath.Join(root, "opencode.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatal(err)
	}
	req := spillRequest(t, root)
	d, err := (opencodeToolOutputs{}).Discover(req)
	if err != nil || len(d.Candidates) != 0 {
		t.Fatalf("before WAL insert: %+v %v", d, err)
	}
	// A bad session cannot prevent a committed reference in a different session from being collected.
	if _, err := db.Exec(`INSERT INTO session_v2 SELECT 'bad',parent_id,fork_session_id,fork_boundary,directory,title,version,time_created,time_updated,cost,tokens_input,tokens_output,tokens_reasoning,tokens_cache_read,tokens_cache_write FROM session_v2 WHERE id='s';
INSERT INTO session_message VALUES ('bad-message','bad','assistant',1,1,1,'{broken');`); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(root, "tool-output", "tool_new")
	quoted, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	data := `{"content":[{"type":"tool","state":{"metadata":{"truncated":true,"outputPath":` + string(quoted) + `}}}]}`
	if _, err := db.Exec(`INSERT INTO session_message VALUES ('new','s','assistant',1,1,1,?)`, data); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "opencode.db-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("fixture has no live WAL: %v", err)
	}
	d, err = (opencodeToolOutputs{}).Discover(req)
	if err != nil || len(d.Candidates) != 1 || d.Unreadable != 1 || !strings.Contains(d.UnreadableReason, "JSON") {
		t.Fatalf("WAL/session isolation: %+v %v", d, err)
	}
	p, err := d.Candidates[0].Load(context.Background())
	if err != nil || string(p.Bytes) != "committed full output" {
		t.Fatalf("WAL result: %s %v", p.Bytes, err)
	}
}
