package sqliteread

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Credentials are returned separately and never pass through the collection row reader.
func CursorAccount(ctx context.Context, path string) (map[string]json.RawMessage, string, error) {
	dbPath := filepath.ToSlash(path)
	if !strings.HasPrefix(dbPath, "/") {
		dbPath = "/" + dbPath
	}
	u := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, "", fmt.Errorf("Cursor account store unavailable")
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT key, value FROM ItemTable WHERE key IN (
 'cursorAuth/accessToken', 'cursorAuth/stripeMembershipType', 'cursorAuth/cachedEmail',
 'cursorAuth/cachedSignUpType', 'cursorAuth/cachedTeam', 'cursorAuth/cachedScopedProfile') AND length(value) <= 1048576`)
	if err != nil {
		return nil, "", fmt.Errorf("Cursor account query failed")
	}
	defer rows.Close()
	values := map[string]json.RawMessage{}
	token := ""
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, "", fmt.Errorf("Cursor account row unreadable")
		}
		if key == "cursorAuth/accessToken" {
			token = value
		} else if json.Valid([]byte(value)) {
			values[key] = json.RawMessage(value)
		} else {
			values[key], _ = json.Marshal(value)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("Cursor account read failed")
	}
	return values, token, nil
}
