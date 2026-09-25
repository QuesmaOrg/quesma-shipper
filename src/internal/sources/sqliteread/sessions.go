package sqliteread

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Session stores only routing metadata; transcript bytes are loaded one session at a time.
type Session struct{ ID, CWD string }

func openSessions(path string) (*sql.DB, error) {
	return sql.Open("sqlite", sessionFileURI(filepath.ToSlash(path))+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
}

// SQLite requires drive letters in the URI path, not the authority (file:///C:/...).
func sessionFileURI(slashPath string) string {
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String()
}

// ListSessions never reads configuration, credentials or arbitrary tables from an agent store.
func ListSessions(ctx context.Context, path, family string) ([]Session, error) {
	db, err := openSessions(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := ""
	switch family {
	case "opencode":
		query = "SELECT id, directory FROM session ORDER BY time_created, id LIMIT 100001"
	case "hermes":
		query = "SELECT id, coalesce(cwd, '') FROM sessions ORDER BY started_at, id LIMIT 100001"
	default:
		return nil, fmt.Errorf("unsupported session family %q", family)
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.CWD); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if len(out) > 100000 {
		return nil, fmt.Errorf("session discovery exceeds 100000 sessions")
	}
	return out, rows.Err()
}

// ReadSession uses one read transaction for metadata, messages and usage, including committed WAL data.
// Only declared columns ship; new database columns cannot silently widen collection.
func ReadSession(ctx context.Context, path, family, id string, maxBytes int64) ([]byte, error) {
	db, err := openSessions(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if maxBytes <= 0 {
		maxBytes = 64 << 20
	}
	var out bytes.Buffer
	emit := func(kind, query string) error {
		rows, err := tx.QueryContext(ctx, query, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			record := map[string]any{"type": kind, "session_id": id, "session_key": sessionKey(family, id), "format_version": 1}
			for i, k := range cols {
				v := vals[i]
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				record[k] = v
			}
			if event, ok := record["id"]; ok {
				record["event_key"] = sessionKey(family+":"+id, fmt.Sprint(event))
			}
			if message, ok := record["message_id"].(string); ok {
				record["message_key"] = sessionKey(family+":"+id, message)
			}
			if kind == "usage" {
				scope, _ := json.Marshal([]any{record["model"], record["billing_provider"], record["billing_base_url"], record["billing_mode"], record["task"]})
				record["usage_id"] = sessionKey(family+":"+id, string(scope))
				delete(record, "billing_base_url")
			}
			// OpenCode's JSON columns are records, not escaped JSON strings.
			if family == "opencode" && kind != "session" {
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
				record["execution_key"] = sessionKey(family+":"+kind, fmt.Sprint(record["time_created"])+":"+string(canonical))
			}
			b, err := json.Marshal(record)
			if err != nil {
				return err
			}
			if int64(out.Len()+len(b)+1) > maxBytes {
				return fmt.Errorf("session exceeds %d-byte cap", maxBytes)
			}
			out.Write(b)
			out.WriteByte('\n')
		}
		return rows.Err()
	}
	switch family {
	case "opencode":
		err = emit("session", `SELECT id, parent_id, directory AS cwd, title, version, time_created, time_updated FROM session WHERE id = ?`)
		if err == nil {
			err = emit("message", `SELECT id, time_created, time_updated, data FROM message WHERE session_id = ? ORDER BY time_created,id`)
		}
		if err == nil {
			err = emit("part", `SELECT id, message_id, time_created, time_updated, data FROM part WHERE session_id = ? ORDER BY time_created,id`)
		}
	case "hermes":
		err = emit("session", `SELECT id, parent_session_id, source, model, cwd, started_at, ended_at, title, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens FROM sessions WHERE id = ?`)
		if err == nil {
			err = emit("message", `SELECT id, role, content, tool_call_id, tool_calls, tool_name, timestamp, finish_reason FROM messages WHERE session_id = ? ORDER BY timestamp,id`)
		}
		if err == nil {
			// Older stores have no per-model counters. Preserve their session totals but do not invent calls.
			var exists int
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_model_usage'`).Scan(&exists)
			if err == nil && exists > 0 {
				err = emit("usage", `SELECT model, billing_provider, billing_base_url, billing_mode, task, api_call_count, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, estimated_cost_usd, actual_cost_usd, cost_status, cost_source, first_seen, last_seen FROM session_model_usage WHERE session_id = ? ORDER BY model,billing_provider,billing_base_url,billing_mode,task`)
			}
		}
	default:
		err = fmt.Errorf("unsupported session family %q", family)
	}
	if err != nil {
		return nil, err
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("session disappeared during collection")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// UUID-shaped keys survive entropy scrubbing; raw agent identifiers may legitimately be redacted.
func sessionKey(namespace, value string) string {
	h := sha256.Sum256([]byte(namespace + "\x00" + value))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
