package sqliteread

import (
	"context"
	"encoding/json"
	"fmt"
)

// OpenCodeSessions reads only OpenCode transcript and usage columns.
type OpenCodeSessions struct{}

// Upstream migration copies 1.x sessions into session_v2 and session_message under the same IDs and keeps the originals.
// A session with 1.x messages ships its 1.x copy, which is the one already archived; any other ships its 2.x copy.
// List and Read pick the same copy, so the directory checked for repository markers is the one exported.
func (OpenCodeSessions) List(ctx context.Context, path string) ([]Session, error) {
	return listSessions(ctx, path, func(tx *sessionConnection) (string, error) {
		tables, err := opencodeTables(ctx, tx)
		if err != nil {
			return "", err
		}
		v1 := `SELECT id, directory AS cwd, time_created FROM session s`
		v2 := `SELECT id, directory AS cwd, time_created FROM session_v2 v`
		switch {
		case tables["session"] && tables["session_v2"]:
			v1 += ` WHERE id NOT IN (SELECT id FROM session_v2) OR EXISTS (SELECT 1 FROM message WHERE session_id = s.id)`
			v2 += ` WHERE id NOT IN (SELECT id FROM session) OR NOT EXISTS (SELECT 1 FROM message WHERE session_id = v.id)`
			v1 += ` UNION ALL ` + v2
		case tables["session_v2"]:
			v1 = v2
		}
		return `SELECT id, cwd FROM (` + v1 + `) ORDER BY time_created, id LIMIT 100001`, nil
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
		var legacy, native bool
		if tables["session"] {
			exists(`SELECT 1 FROM message WHERE session_id = ? LIMIT 1`, &legacy)
		}
		if tables["session_message"] {
			exists(`SELECT 1 FROM session_message WHERE session_id = ? LIMIT 1`, &native)
		}
		inV2 := false
		if tables["session_v2"] {
			exists(`SELECT 1 FROM session_v2 WHERE id = ?`, &inV2)
		}
		// V2 keeps the message kind in a column and orders by seq; time_created can tie.
		messages := `SELECT id, type AS message_type, seq, time_created, time_updated, data FROM session_message WHERE session_id = ? ORDER BY seq`
		v1Session := `SELECT id, parent_id, directory AS cwd, title, version, time_created, time_updated FROM session WHERE id = ?`
		switch {
		case err != nil:
		case !legacy && (inV2 || !tables["session"]):
			err = emit("session", `SELECT id, parent_id, fork_session_id, fork_boundary, directory AS cwd, title, version, time_created, time_updated FROM session_v2 WHERE id = ?`)
			if err == nil {
				err = emit("session_message", messages)
			}
		case native && !legacy:
			// The shared 2.x layout keeps 1.x session rows but writes their messages to session_message.
			err = emit("session", v1Session)
			if err == nil {
				err = emit("session_message", messages)
			}
		default:
			err = emit("session", v1Session)
			if err == nil {
				err = emit("message", `SELECT id, time_created, time_updated, data FROM message WHERE session_id = ? ORDER BY time_created,id`)
			}
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
			scope := "opencode:" + kind
			// V2 keeps the message kind outside data, so it must separate otherwise identical records.
			if messageType, ok := record["message_type"].(string); ok {
				scope += ":" + messageType
			}
			record["execution_key"] = sessionKey(scope, fmt.Sprint(record["time_created"])+":"+string(canonical))
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
