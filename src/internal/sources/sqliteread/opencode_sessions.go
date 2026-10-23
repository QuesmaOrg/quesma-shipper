package sqliteread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
)

// OpenCodeSessions reads only OpenCode transcript and usage columns.
type OpenCodeSessions struct{}

func (OpenCodeSessions) List(ctx context.Context, path string) ([]Session, error) {
	sessions, err := listSessions(ctx, path, `SELECT id, directory AS cwd FROM session ORDER BY time_created, id LIMIT 100001`)
	main := filepath.Join(filepath.Dir(path), "opencode.db")
	if err != nil || filepath.Base(path) != "opencode-next.db" {
		return sessions, err
	}
	if _, err := os.Lstat(main); errors.Is(err, fs.ErrNotExist) {
		return sessions, nil
	}
	// OpenCode 2.x imports opencode-next.db into opencode.db and leaves it behind; a 1.x "next" channel, or an import
	// that has not run or finished, leaves sessions with no other copy.
	imported, err := opencodeSessionIDs(ctx, main)
	return slices.DeleteFunc(sessions, func(s Session) bool { return imported[s.ID] }), err
}

func opencodeSessionIDs(ctx context.Context, path string) (map[string]bool, error) {
	db, err := openSessions(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var tables []string
	err = db.query(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name IN ('session','session_v2')`, nil, func(row map[string]any) error {
		tables = append(tables, row["name"].(string))
		return nil
	})
	ids := map[string]bool{}
	for _, table := range tables {
		if err == nil {
			err = db.query(ctx, `SELECT id FROM `+table, nil, func(row map[string]any) error {
				id, _ := row["id"].(string)
				ids[id] = true
				return nil
			})
		}
	}
	return ids, err
}

func (OpenCodeSessions) Read(ctx context.Context, path, relPath, id string, maxBytes int64) ([]byte, error) {
	namespace := "opencode"
	// Channel databases (opencode-<channel>.db) are separate histories that may reuse session IDs.
	if relPath = filepath.ToSlash(filepath.Clean(relPath)); relPath != "opencode.db" {
		namespace += ":" + sessionKey("opencode-database", relPath)
	}
	return readSession(ctx, path, namespace, id, maxBytes, func(tx *sessionConnection, emit func(string, string) error) error {
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
