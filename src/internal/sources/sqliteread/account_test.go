package sqliteread_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/sources/sqliteread"
)

func TestCursorCredentialReadIsSeparateFromCollectedRows(t *testing.T) {
	path := newStore(t, map[string]string{
		"cursorAuth/accessToken":         "fixture-access",
		"cursorAuth/refreshToken":        "fixture-refresh",
		"cursorAuth/cachedEmail":         "dev@example.org",
		"cursorAuth/cachedTeam":          `{"name":"team","future":9007199254740993}`,
		"cursorAuth/cachedScopedProfile": `{"future":null}`,
		"cursorAuth/futureSecret":        "not-collected",
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	values, token, err := sqliteread.CursorAccount(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if token != "fixture-access" || len(values) != 3 {
		t.Fatalf("unexpected metadata count %d", len(values))
	}
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"fixture-access", "fixture-refresh", "not-collected"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatal("credential entered metadata")
		}
	}
	if !bytes.Contains(raw, []byte(`"future":9007199254740993`)) {
		t.Fatal("provider field lost")
	}
	generic, err := sqliteread.Read(sqliteread.Options{Path: path, ScratchDir: t.TempDir(), Table: "ItemTable", KeyPrefixes: []string{"cursorAuth/"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range generic.Rows {
		if row.Key == "cursorAuth/accessToken" || row.Key == "cursorAuth/refreshToken" {
			t.Fatal("generic collection acquired credentials")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("account read changed store", err)
	}
	missing := filepath.Join(t.TempDir(), "absent.vscdb")
	if _, _, err := sqliteread.CursorAccount(context.Background(), missing); err == nil {
		t.Fatal("missing store succeeded")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("read created database")
	}
}
