package transforms

import (
	"encoding/json"
	"strings"
	"testing"
)

// The key-name rule once required a prefix before the secret word (`*_PASSWORD` matched
// `DB_PASSWORD` but not `PASSWORD`) and could not see past the closing quote of a JSON key,
// so both shapes below shipped their values in clear.
func TestKeyNameRuleRedactsBareNamesAndQuotedKeys(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	userLine := func(text string) string {
		quoted, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		return `{"type":"user","uuid":"1","message":{"role":"user","content":[{"type":"text","text":` +
			string(quoted) + `}]}}` + "\n"
	}
	pretty := "{\n  \"env\": {\n    \"ANTHROPIC_AUTH_TOKEN\": \"my-proxy-token-123\",\n    \"MY_PASSWORD\": \"hunter2\"\n  }\n}\n"
	cases := []struct {
		name    string
		payload string
		jsonl   bool
		secrets []string
	}{
		{"export PASSWORD", userLine("export PASSWORD=hunter2 && ./run"), true, []string{"hunter2"}},
		{"SECRET=", userLine("SECRET=s3cr3t-value-here"), true, []string{"s3cr3t-value-here"}},
		{"APIKEY=", userLine("APIKEY=zzzz-yyyy-xxxx"), true, []string{"zzzz-yyyy-xxxx"}},
		{"yaml password", userLine("db:\n  password: hunter2\n"), true, []string{"hunter2"}},
		{"pretty settings.json, raw", pretty, false, []string{"my-proxy-token-123", "hunter2"}},
		{"pretty settings.json, jsonl hint", pretty, true, []string{"my-proxy-token-123", "hunter2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := s.Scrub([]byte(c.payload), Hint{Family: "claude-code", JSONL: c.jsonl})
			if err != nil {
				t.Fatal(err)
			}
			out := string(res.Out.Bytes())
			for _, secret := range c.secrets {
				if strings.Contains(out, secret) {
					t.Errorf("%q survived scrubbing:\n%s", secret, out)
				}
			}
		})
	}
}

// What the wider rule must still leave alone: words that only contain a secret word, and a
// secret word with no value after it.
func TestKeyNameRuleLeavesNonAssignmentsAlone(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"tokens: 4096",
		"passwords are rotated weekly",
		"the SECRET is safe",
		"MYTOKEN=visible-on-purpose",
	} {
		res, err := s.Scrub([]byte(text+"\n"), Hint{Family: "claude-code"})
		if err != nil {
			t.Fatal(err)
		}
		if got := string(res.Out.Bytes()); got != text+"\n" {
			t.Errorf("%q changed to %q", text, got)
		}
	}
}

// Embedded JSON that encoding/json rejects is scanned as text, and the text scan could not see
// past a quoted key: a password inside a malformed tool output shipped in clear.
func TestMalformedEmbeddedJSONDoesNotLeakQuotedSecrets(t *testing.T) {
	s, err := New(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []string{`[,`, `{"a":}`, `[01]`, `{"a":1,}`} {
		inner := `[` + doc + `,{"password":"hunter2"}]`
		line, err := json.Marshal(map[string]string{"out": inner})
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.Scrub(append(line, '\n'), Hint{Family: "codex", JSONL: true})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(res.Out.Bytes()), "hunter2") {
			t.Errorf("%s: the password survived: %s", inner, res.Out.Bytes())
		}
	}
}
