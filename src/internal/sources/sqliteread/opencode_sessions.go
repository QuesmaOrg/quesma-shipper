package sqliteread

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// OpenCodeSessions reads only OpenCode transcript and usage columns.
type OpenCodeSessions struct{}

func (OpenCodeSessions) List(ctx context.Context, path string) ([]Session, error) {
	return listSessions(ctx, path, `SELECT id, directory FROM session ORDER BY time_created, id LIMIT 100001`)
}
func (OpenCodeSessions) Read(ctx context.Context, path, id string, maxBytes int64) ([]byte, error) {
	return readSession(ctx, path, "opencode", id, maxBytes, func(tx *sql.Tx, emit func(string, string) error) error {
		err := emit("session", `SELECT id, parent_id, directory AS cwd, title, version, time_created, time_updated FROM session WHERE id = ?`)
		if err == nil {
			err = emit("message", `SELECT id, time_created, time_updated, data FROM message WHERE session_id = ? ORDER BY time_created,id`)
		}
		if err == nil {
			err = emit("part", `SELECT id, message_id, time_created, time_updated, data FROM part WHERE session_id = ? ORDER BY time_created,id`)
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
