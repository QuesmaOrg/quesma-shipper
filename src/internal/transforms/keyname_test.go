package transforms

import (
	"strings"
	"testing"
)

// The key-name rule is the only backstop for a credential with no recognisable shape: letters
// and digits, in a field whose name says what it is.
func TestFieldNamesThatSayTheirValueIsACredential(t *testing.T) {
	m := newKeyNameMatcher(DefaultSecretKeyNames())

	for _, key := range []string{
		// All of these shipped in the clear.
		"api_key", "apiKey", "API_KEY", "x-api-key", "apikey",
		"password", "Password", "passwd", "pwd",
		"token", "Token", "authorization", "Authorization", "cookie",
		"secret", "private_key", "privateKey", "credentials", "auth",
		"aws.secret_access_key", "signing_key", "encryption_key",

		// These matched before and must keep matching.
		"access_token", "client_secret", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "MY_TOKEN",
	} {
		if !m.MatchesKeyName(key) {
			t.Errorf("%q does not name a secret, and it does", key)
		}
	}
}

// The dangerous half: a rule firing on anything containing "token" would redact every usage
// figure in every transcript, which is the product, and one firing on "id" or "key" would take
// session ids and object keys with it.
func TestFieldNamesThatOnlyLookLikeCredentials(t *testing.T) {
	m := newKeyNameMatcher(DefaultSecretKeyNames())

	for _, key := range []string{
		// Counts. Every token number the ETL reports arrives under one of these.
		"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens",
		"max_tokens", "total_tokens", "token_count", "tokenCount", "usage",

		// Identifiers, not credentials.
		"sessionId", "session_id", "install_id", "request_id", "uuid", "toolUseId",

		// Ordinary fields that end in a word the rule cares about only in context.
		"object_key", "cache_key", "keyRoot", "model", "content", "type",

		// A setting about secrets is not a secret.
		"secret_count", "secret_scanning_enabled",
	} {
		if m.MatchesKeyName(key) {
			t.Errorf("%q is not a credential and would be redacted", key)
		}
	}
}

func TestKeyNamesSplitOnSeparatorsAndCamelHumps(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"api_key", []string{"api", "key"}},
		{"apiKey", []string{"api", "key"}},
		{"x-api-key", []string{"x", "api", "key"}},
		{"AWSSecretKey", []string{"aws", "secret", "key"}},
		{"aws.secret_access_key", []string{"aws", "secret", "access", "key"}},
		{"HTTPToken", []string{"http", "token"}},
		{"", nil},
	} {
		got := splitKeyWords(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("%q -> %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q -> %v, want %v", tc.in, got, tc.want)
				break
			}
		}
	}
}

// The configured-name compare folds ASCII in place; strings.ToUpper, the fold it replaced,
// stays here as the oracle. The corners: bytes next to the fold range, and non-ASCII runes
// whose uppercase changes length or lands on ASCII.
func TestConfiguredKeyNameFoldMatchesToUpper(t *testing.T) {
	reference := func(m *keyNameMatcher, key string) bool {
		if key == "" {
			return false
		}
		if matchesSecretKeyName(key) {
			return true
		}
		upper := strings.ToUpper(key)
		for _, n := range m.names {
			if n.suffix && strings.HasSuffix(upper, n.upper) || !n.suffix && upper == n.upper {
				return true
			}
		}
		return false
	}

	names := append(DefaultSecretKeyNames(),
		"STRASSE", "*_SS", "ſTAGE", "*_ﬀ", "DIẞ", "İD", "KKY", "A@[B", "*_Z`", "*", "")

	keys := []string{
		"", "a", "A", "@", "[", "`", "{", "a@[b", "A@[B", "a`{b", "A`{B", "x_z`", "X_Z`", "x_Z@",
		"strasse", "Strasse", "straße", "STRAẞE", "x_ss", "x_ß", "X_SS", "x_ẞ",
		"x_ﬀ", "x_ff", "X_FF", "X_ﬀ", "ﬀ", "stage", "ſtage", "ſTAGE", "STAGE",
		"diß", "DIẞ", "diẞ", "DISS", "İd", "id", "iD", "ıd", "ID", "i̇d",
		"kkey", "kKy", "KKY", "KKY", "kky",
		"database_url", "Database_Url", "DATABASE_URL", "DATABASE_URL ", "xDATABASE_URL",
		"my_token", "My_Token", "ı_token", "x_tokEn", "_TOKEN", "TOKEN_", "_token\xff", "a\xffb_ss",
		"\xff", "\xc3", "é_passwd", "É_PASSWD",
	}
	for _, n := range names {
		lit := strings.TrimPrefix(n, "*")
		keys = append(keys, lit, strings.ToLower(lit), "x_"+lit, "X_"+strings.ToLower(lit), lit+"_x")
	}
	// One matcher per name, so a catch-all like "*" cannot mask the others.
	matched := 0
	for _, n := range names {
		m := newKeyNameMatcher([]string{n})
		for _, key := range keys {
			got, want := m.MatchesKeyName(key), reference(m, key)
			if got != want {
				t.Errorf("name %q: MatchesKeyName(%q) = %v, strings.ToUpper reference = %v", n, key, got, want)
			}
			if want && !isASCII(key) && !matchesSecretKeyName(key) {
				matched++
			}
		}
	}
	if matched == 0 {
		t.Error("no non-ASCII key matched a configured name; the fallback went untested")
	}
}

// An operator naming their own field must be able to add it: the config field existed and
// reached nothing.
func TestAConfiguredNameIsHonoured(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SecretKeyNames = append(cfg.SecretKeyNames, "ACME_DEPLOY_SIG")
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Scrub([]byte(`{"ACME_DEPLOY_SIG":"zx81-plain-value"}`+"\n"),
		Hint{Family: "claude-code", JSONL: true})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Out) == `{"ACME_DEPLOY_SIG":"zx81-plain-value"}`+"\n" {
		t.Errorf("a configured secret key name did not redact:\n%s", res.Out)
	}
}
