package transforms

import "testing"

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
