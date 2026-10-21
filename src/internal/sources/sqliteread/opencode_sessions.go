package sqliteread

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// OpenCodeSessions reads only OpenCode transcript and usage columns.
type OpenCodeSessions struct{}

func (OpenCodeSessions) List(ctx context.Context, path string) ([]Session, error) {
	return listSessions(ctx, path, func(tx *sessionConnection) (string, error) {
		tables, err := opencodeTables(ctx, tx)
		if err != nil {
			return "", err
		}
		var from []string
		for _, table := range []string{"session", "session_v2"} {
			if tables[table] {
				from = append(from, "SELECT id, directory AS cwd, time_created FROM "+table)
			}
		}
		return `SELECT id, cwd FROM (` + strings.Join(from, " UNION ALL ") + `) GROUP BY id ORDER BY min(time_created), id LIMIT 100001`, nil
	})
}
func (OpenCodeSessions) Read(ctx context.Context, path, _, id string, maxBytes int64) ([]byte, error) {
	return readSession(ctx, path, "opencode", id, maxBytes, func(tx *sessionConnection, emit func(string, string) error) error {
		tables, err := opencodeTables(ctx, tx)
		exists := func(query string, found *bool) {
			if err == nil {
				err = tx.query(ctx, query, []string{id}, func(map[string]any) error { *found = true; return nil })
			}
		}
		// Upstream migration copies 1.x sessions into session_v2 and session_message under the same IDs and keeps the originals: ship one copy, the 1.x one.
		var inV1, legacy, inV2, native bool
		if tables["session"] {
			exists(`SELECT 1 FROM session WHERE id = ?`, &inV1)
			exists(`SELECT 1 FROM message WHERE session_id = ? LIMIT 1`, &legacy)
		}
		if tables["session_v2"] {
			exists(`SELECT 1 FROM session_v2 WHERE id = ?`, &inV2)
		}
		if tables["session_message"] {
			exists(`SELECT 1 FROM session_message WHERE session_id = ? LIMIT 1`, &native)
		}
		switch {
		case err != nil:
		case inV2 && !inV1:
			err = emit("session", `SELECT id, parent_id, fork_session_id, fork_boundary, directory AS cwd, title, version, time_created, time_updated FROM session_v2 WHERE id = ?`)
		case tables["session"]:
			err = emit("session", `SELECT id, parent_id, directory AS cwd, title, version, time_created, time_updated FROM session WHERE id = ?`)
		}
		switch {
		case err != nil:
		case native && !legacy:
			// V2 keeps the message kind in a column and orders by seq; time_created can tie.
			err = emit("session_message", `SELECT id, type AS message_type, seq, time_created, time_updated, data FROM session_message WHERE session_id = ? ORDER BY seq`)
		case tables["session"]:
			err = emit("message", `SELECT id, time_created, time_updated, data FROM message WHERE session_id = ? ORDER BY time_created,id`)
			if err == nil {
				err = emit("part", `SELECT id, message_id, time_created, time_updated, data FROM part WHERE session_id = ? ORDER BY time_created,id`)
			}
		}

		return err
	}, func(kind string, record map[string]any) error {
		// OpenCode's JSON columns are records, not escaped JSON strings.
		if kind != "session" {
			raw, ok := record["data"].(string)
			if !ok || !json.Valid([]byte(raw)) {
				return fmt.Errorf("invalid %s JSON", kind)
			}
			record["data"] = json.RawMessage(raw)
			var identity map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &identity); err != nil {
				return err
			}
			for _, key := range []string{"id", "sessionID", "messageID", "parentID"} {
				delete(identity, key)
			}
			canonical, err := json.Marshal(identity)
			if err != nil {
				return err
			}
			record["execution_key"] = sessionKey("opencode:"+kind, fmt.Sprint(record["time_created"])+":"+string(canonical))
		}

		return nil
	})
}

// opencodeTables accepts V1 (session, message, part), V2 (session_v2, session_message), and migrated stores holding both.
// A partial layout fails loudly rather than shipping sessions without their transcript.
func opencodeTables(ctx context.Context, tx *sessionConnection) (map[string]bool, error) {
	tables := map[string]bool{}
	err := tx.query(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session','message','part','session_v2','session_message')`, nil, func(row map[string]any) error {
		tables[row["name"].(string)] = true
		return nil
	})
	v1 := tables["session"] && tables["message"] && tables["part"]
	v2 := tables["session_v2"] && tables["session_message"]
	partial := tables["session"] && !v1 || tables["session_v2"] && !v2
	if err == nil && (partial || !v1 && !v2) {
		err = fmt.Errorf("unsupported OpenCode database schema: want session, message and part, or session_v2 and session_message tables")
	}
	return tables, err
}
