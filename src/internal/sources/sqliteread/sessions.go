package sqliteread

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// DefaultSessionMaxBytes bounds snapshots when no positive cap is supplied.
const DefaultSessionMaxBytes = 64 << 20

// Session stores only routing metadata; transcript bytes are loaded one session at a time.
type Session struct{ ID, CWD string }

// SQLite requires drive letters in the URI path, not the authority (file:///C:/...).
func sessionFileURI(slashPath string) string {
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return (&url.URL{Scheme: "file", Path: slashPath}).String()
}

// listSessions bounds routing metadata independently of transcript loading.
func listSessions(ctx context.Context, path, query string) ([]Session, error) {
	db, err := openSessions(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var out []Session
	err = db.query(ctx, query, nil, func(row map[string]any) error {
		id, okID := row["id"].(string)
		cwd, okCWD := row["cwd"].(string)
		if !okID || !okCWD {
			return fmt.Errorf("invalid session routing metadata")
		}
		out = append(out, Session{ID: id, CWD: cwd})
		if len(out) > 100000 {
			return fmt.Errorf("session discovery exceeds 100000 sessions")
		}
		return nil
	})
	return out, err
}

// readSession uses one read transaction for metadata, messages and usage, including committed WAL data.
// Only declared columns ship; new database columns cannot silently widen collection.
func readSession(ctx context.Context, path, family, id string, maxBytes int64, collect func(*sessionConnection, func(string, string) error) error, decorate func(string, map[string]any) error) ([]byte, error) {
	db, err := openSessions(ctx, path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if maxBytes <= 0 {
		maxBytes = DefaultSessionMaxBytes
	}
	var out bytes.Buffer
	emit := func(kind, query string) error {
		return db.query(ctx, query, []string{id}, func(record map[string]any) error {
			record["type"], record["session_id"], record["session_key"], record["format_version"] = kind, id, sessionKey(family, id), 1
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
			return nil
		})
	}
	err = collect(db, emit)
	if err != nil {
		return nil, err
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("session disappeared during collection")
	}
	return out.Bytes(), nil
}

// UUID-shaped keys survive entropy scrubbing; raw agent identifiers may legitimately be redacted.
func sessionKey(namespace, value string) string {
	h := sha256.Sum256([]byte(namespace + "\x00" + value))
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[:4], h[4:6], h[6:8], h[8:10], h[10:16])
}
