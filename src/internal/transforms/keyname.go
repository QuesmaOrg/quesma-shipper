package transforms

import "strings"

// A field name a program chose (`api_key`, `apiKey`, `x-api-key`) is compared as WORDS, split
// on separators and camelCase humps. As a suffix of the configured environment names it would
// not be covered at all: `*_API_KEY` compiles to `_API_KEY`, which `API_KEY` does not end with.

// secretWords mean "the value is a credential" as the LAST word of a key. Last word, not any
// word: `secret_scanning_enabled` is a setting and `token_count` is a number.
var secretWords = map[string]bool{
	"password":    true,
	"passwd":      true,
	"pwd":         true,
	"secret":      true,
	"token":       true,
	"apikey":      true,
	"credential":  true,
	"credentials": true,
	"auth":        true,
	// The HTTP header carries the token verbatim, and a logged request is the most common
	// way a credential reaches a transcript.
	"authorization": true,
	"cookie":        true,
	// "session" is deliberately absent: alone it is an id, not a credential.
}

// secretTails are two-word endings: "key" alone is too common (object_key, cache_key), so it
// counts only when the word before says which kind. "id" never qualifies, whatever precedes it.
var secretTails = map[string][]string{
	"key": {"api", "secret", "private", "access", "signing", "encryption", "auth"},
}

// matchesSecretKeyName reports whether a field name says its value is a credential.
func matchesSecretKeyName(key string) bool {
	words := splitKeyWords(key)
	if len(words) == 0 {
		return false
	}
	last := words[len(words)-1]

	// The exact word only, never a prefix or a contains: input_tokens and max_tokens are
	// counts, and redacting them would redact the product.
	// TestFieldNamesThatOnlyLookLikeCredentials holds that line.
	if secretWords[last] {
		return true
	}
	if qualifiers, ok := secretTails[last]; ok && len(words) >= 2 {
		prev := words[len(words)-2]
		for _, q := range qualifiers {
			if prev == q {
				return true
			}
		}
	}
	return false
}

// splitKeyWords normalizes a field name into lowercase words: separators and camelCase humps
// break words, and a run of capitals stays together so `AWSSecretKey` yields ["aws","secret",
// "key"]. Bytes, not runes: every predicate is ASCII-only, so a continuation byte breaks nothing.
func splitKeyWords(key string) []string {
	// Room for eight words, which covers a real field name in one allocation.
	var buf [8]string
	words := buf[:0]
	start := -1

	flush := func(end int) {
		if start >= 0 {
			words = append(words, strings.ToLower(key[start:end]))
			start = -1
		}
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '_' || c == '-' || c == '.' || c == ' ' || c == ':' || c == '/':
			flush(i)
		case isUpper(c) && i > 0 && (isLower(key[i-1]) || isDigit(key[i-1])):
			// apiKey -> api | Key
			flush(i)
			start = i
		case isUpper(c) && i > 0 && isUpper(key[i-1]) &&
			i+1 < len(key) && isLower(key[i+1]):
			// AWSSecret -> AWS | Secret
			flush(i)
			start = i
		default:
			if start < 0 {
				start = i
			}
		}
	}
	flush(len(key))
	return words
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }
func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isDigit(c byte) bool { return c >= '0' && c <= '9' }
