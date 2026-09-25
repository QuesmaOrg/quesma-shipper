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

// listSessions bounds routing metadata independently of transcript loading.
func listSessions(ctx context.Context, path, query string) ([]Session, error) {
	db, err := openSessions(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
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

// readSession uses one read transaction for metadata, messages and usage, including committed WAL data.
// Only declared columns ship; new database columns cannot silently widen collection.
func readSession(ctx context.Context, path, family, id string, maxBytes int64, collect func(*sql.Tx, func(string, string) error) error, decorate func(string, map[string]any) error) ([]byte, error) {
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
			if err := decorate(kind, record); err != nil {
				return err
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
	err = collect(tx, emit)
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
