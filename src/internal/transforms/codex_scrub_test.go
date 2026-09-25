package transforms_test

// Codex records tool arguments and outputs as JSON documents encoded inside a JSON string, so a
// decoded value still carries `\n` escapes (gap B) and nested object keys (gap C). Claude Code
// records the same data as real JSON, which is the control. All credentials are seeded-random.

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

func codexJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func codexLine(t *testing.T, typ string, payload map[string]any) string {
	return codexJSON(t, map[string]any{"timestamp": "2026-09-20T10:00:00.000Z", "type": typ, "payload": payload}) + "\n"
}

// codexToolOutput is function_call_output.output: itself a JSON document, as in older rollouts.
func codexToolOutput(t *testing.T, stdout string) string {
	return codexJSON(t, map[string]any{"output": stdout, "metadata": map[string]any{"exit_code": 0, "duration_seconds": 0.1}})
}

func assertScrubbed(t *testing.T, family, line string, secrets ...string) {
	t.Helper()
	res := scrubJSONL(t, newScrubber(t), family, line)
	for _, sec := range secrets {
		if strings.Contains(string(res.Out.Bytes()), sec) {
			t.Errorf("%s: %q survived scrubbing\nout: %s\nhits: %v", family, sec, res.Out.Bytes(), res.RuleHits)
		}
	}
}

// assertArgumentsScrubbed returns a scrubbed function_call's arguments, still JSON and free of secrets.
func assertArgumentsScrubbed(t *testing.T, line string, secrets ...string) string {
	t.Helper()
	res := scrubJSONL(t, newScrubber(t), "codex", line)
	var out struct {
		Payload struct{ Arguments string } `json:"payload"`
	}
	if err := json.Unmarshal(res.Out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	args := out.Payload.Arguments
	if !json.Valid([]byte(args)) {
		t.Errorf("encoded arguments no longer JSON: %s", args)
	}
	for _, sec := range secrets {
		if strings.Contains(args, sec) {
			t.Errorf("%q survived: %s (hits %v)", sec, args, res.RuleHits)
		}
	}
	return args
}

// A `\n` or `\t` escape left in a decoded value glues a word byte onto the token, hiding it from `\b`-anchored rules.
func TestCodexEscapedWhitespaceBeforeTokenIsScrubbed(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	tokens := map[string]string{
		"aws-access-key-id":  "AKIA" + randFrom(r, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", 16),
		"digitalocean-token": "dop_v1_" + randHex(r, 64),
		"databricks":         "dapi" + randHex(r, 32),
		"shopify-token":      "shpat_" + randHex(r, 32),
		"pulumi":             "pul-" + randHex(r, 40),
		"supabase":           "sbp_" + randHex(r, 40),
		"twilio":             "SK" + randHex(r, 32),
	}
	execCommand := func(cmd string) string {
		return codexLine(t, "response_item", map[string]any{"type": "function_call", "name": "exec_command", "call_id": "call_abc123",
			"arguments": codexJSON(t, map[string]any{"cmd": cmd, "workdir": "/Users/jane/proj"})})
	}
	for rule, tok := range tokens {
		shapes := map[string]string{
			"plain_output_control": codexLine(t, "response_item", map[string]any{"type": "function_call_output", "call_id": "call_abc123",
				"output": "Command: /bin/zsh -lc 'cat token.txt'\nProcess exited with code 0\nOutput:\n" + tok + "\n"}),
			"legacy_output": codexLine(t, "response_item", map[string]any{"type": "function_call_output", "call_id": "call_abc123",
				"output": codexToolOutput(t, "token\n"+tok+"\n")}),
			// Looks like a document but does not parse, so no inner walk decodes the escape.
			"truncated_json_output": codexLine(t, "response_item", map[string]any{"type": "function_call_output", "call_id": "call_abc123",
				"output": `{"stdout":"token\n` + tok}),
			"exec_command_heredoc": execCommand("cat > .secrets <<'EOF'\n# " + rule + "\n" + tok + "\nEOF"),
			"exec_command_tab":     execCommand("printf 'name\\t%s\\n' ok\nprintf 'ci\t" + tok + "\\n' >> tokens.tsv"),
		}
		for shape, line := range shapes {
			t.Run(rule+"/"+shape, func(t *testing.T) { assertScrubbed(t, "codex", line, tok) })
		}
	}
}

// Key-name redaction reaches arguments Codex stores as a JSON-encoded string, as it does Claude Code's objects.
func TestCodexKeyNamesInsideJSONStringsAreScrubbed(t *testing.T) {
	for _, secret := range []string{"correct-horse-battery-staple-42", "Xq9vT2mLp4Rz8Kw1", "hunter2hunter2"} {
		args := map[string]any{"host": "db.internal", "user": "admin", "password": secret, "api_key": secret, "client_secret": secret}
		claude := codexJSON(t, map[string]any{"type": "assistant", "uuid": "u1", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "mcp__db__connect", "input": args}}}}) + "\n"
		codex := codexLine(t, "response_item", map[string]any{"type": "function_call", "name": "mcp__db__connect", "call_id": "call_1",
			"arguments": codexJSON(t, args)})
		t.Run("claude-code_control/"+secret, func(t *testing.T) { assertScrubbed(t, "claude-code", claude, secret) })
		t.Run("codex_function_call/"+secret, func(t *testing.T) { assertScrubbed(t, "codex", codex, secret) })
	}
}

// Key names reach into `cat config.json` output, a JSON document one or more string-encodings deep.
func TestCodexCatConfigJSONIsScrubbed(t *testing.T) {
	const password = "correct-horse-battery-staple-42"
	const clientSecret = "Xq9vT2mLp4Rz8Kw1"
	config := "{\n  \"db\": {\"user\": \"app\", \"password\": \"" + password + "\"},\n  \"oauth\": {\"client_secret\": \"" + clientSecret + "\"}\n}\n"
	cases := map[string]string{
		"function_call_output": codexLine(t, "response_item", map[string]any{"type": "function_call_output", "call_id": "call_abc",
			"output": codexToolOutput(t, config)}),
		"custom_tool_call_output_nested_twice": codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "call_abc",
			"output": codexJSON(t, map[string]any{"output": codexToolOutput(t, config)})}),
		"exec_command_end": codexLine(t, "event_msg", map[string]any{"type": "exec_command_end", "call_id": "call_abc",
			"stdout": config, "aggregated_output": config, "exit_code": 0}),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) { assertScrubbed(t, "codex", line, password, clientSecret) })
	}
}

// An entropy hit right after a `\n` escape must take the whole escape: a lone `\` left behind
// breaks the encoded document, and the keys walked inside it would then ship unredacted.
func TestCodexEntropyHitAfterEscapeKeepsEncodedJSONValid(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	token := randToken(r, 40)
	line := codexLine(t, "response_item", map[string]any{"type": "function_call", "name": "mcp__db__connect", "call_id": "call_1",
		"arguments": codexJSON(t, map[string]any{"api_key": "hunter2word", "text": "line\n" + token})})
	assertArgumentsScrubbed(t, line, "hunter2word", token)
}

// No whole-value scan reads an encoded document the inner walk completes on, so its numbers get
// the ladder there, and a redacted one becomes a string that keeps the document valid.
func TestCodexNumbersInsideJSONStringsAreScrubbed(t *testing.T) {
	const pin, pan = "12345678901234", "4111111111111111"
	line := codexLine(t, "response_item", map[string]any{"type": "function_call", "name": "mcp__db__connect", "call_id": "call_1",
		"arguments": `{"password": ` + pin + `, "card": ` + pan + `, "port": 5432}`})
	args := assertArgumentsScrubbed(t, line, pin, pan)
	if !strings.Contains(args, `"port": 5432`) {
		t.Errorf("a clean number was rewritten: %s", args)
	}
}

// A rule that reads a key with its value never sees both on a leaf, so the walk must not replace
// the whole-value scan for such a document. The secret is low-entropy: no backstop catches it.
func TestCodexKeyPlusValueRulesInsideJSONStrings(t *testing.T) {
	const secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	for _, c := range []struct{ args, ruleID string }{
		{`{"AwsAccessKeyID":"` + secret + `"}`, "aws-secret-key"},
		{`{"Credentials": {"SecretAccessKey": "` + secret + `"}}`, "secret-access-key"},
	} {
		line := codexLine(t, "response_item", map[string]any{"type": "function_call", "name": "shell", "call_id": "call_1", "arguments": c.args})
		res := scrubJSONL(t, newScrubber(t), "codex", line)
		if strings.Contains(string(res.Out.Bytes()), "EXAMPLEKEY") {
			t.Errorf("%s: secret survived: %s (hits %v)", c.args, res.Out.Bytes(), res.RuleHits)
		}
		if res.RuleHits[c.ruleID] == 0 {
			t.Errorf("%s: %s did not fire: %v", c.args, c.ruleID, res.RuleHits)
		}
	}
}
