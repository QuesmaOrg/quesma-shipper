package sqliteread

import (
	"context"
	"database/sql"
	"encoding/json"
)

// HermesSessions reads only Hermes transcript and usage columns.
type HermesSessions struct{}

func (HermesSessions) List(ctx context.Context, path string) ([]Session, error) {
	return listSessions(ctx, path, `SELECT id, coalesce(cwd, '') FROM sessions ORDER BY started_at, id LIMIT 100001`)
}
func (HermesSessions) Read(ctx context.Context, path, id string, maxBytes int64) ([]byte, error) {
	return readSession(ctx, path, "hermes", id, maxBytes, func(tx *sql.Tx, emit func(string, string) error) error {
		err := emit("session", `SELECT id, parent_session_id, source, model, cwd, started_at, ended_at, title, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens FROM sessions WHERE id = ?`)
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

		return err
	}, func(kind string, record map[string]any) error {
		if kind == "usage" {
			scope, _ := json.Marshal([]any{record["model"], record["billing_provider"], record["billing_base_url"], record["billing_mode"], record["task"]})
			record["usage_id"] = sessionKey("hermes:"+id, string(scope))
			delete(record, "billing_base_url")
		}

		return nil
	})
}
