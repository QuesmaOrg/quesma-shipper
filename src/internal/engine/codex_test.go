package engine_test

// Codex through the real catalog and config.Resolve: a cold rollout (.jsonl.zst) is scrubbed like
// a live one, and a session is one bucket object keyed by its UUID however Codex moves or
// compresses it. Credentials are synthetic.

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

var codexSecrets = map[string]string{
	"openai":    "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKL",
	"anthropic": "sk-ant-api03-abcdefghijklmnopqrstuvwxyz012345-6789ABCDEFGHIJKLMN",
	"ghp":       "ghp_abcdefghijklmnopqrstuvwxyz0123456789",
	"ghfg":      "github_pat_11ABCDEFG0abcdefghijklmnop_abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNO",
	"akia":      "AKIAIOSFODNN7EXAMPLE",
	"awssecret": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	"jwt":       "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkphbmUifQ.abcdefghijklmnopqrstuvwxyz0123456789",
	"bearer":    "tok9Xq8Wm3Xv7Kp2Rt9Ly4Nc6Hb1Jd5Fg0Se",
	"slack":     "xoxb-123456789012-1234567890123-abcdefghijklmnopqrstuvwx",
	"stripe":    "sk_live_abcdefghijklmnopqrstuvwx",
}

const (
	codexUUID1 = "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0001"
	codexUUID2 = "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0002"
	codexUUID3 = "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0003"
)

// codexResumed is a line a resumed session appends.
const codexResumed = `{"timestamp":"2026-09-02T10:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"resumed"}}` + "\n"

func codexRolloutName(uuid string) string { return "rollout-2026-09-01T10-00-00-" + uuid + ".jsonl" }

// codexRollout covers the event shapes a rollout carries credentials in.
func codexRollout(uuid string) string {
	s := codexSecrets
	env := `OPENAI_API_KEY=` + s["openai"] + `\nANTHROPIC_API_KEY=` + s["anthropic"] + `\nGITHUB_TOKEN=` + s["ghp"] +
		`\nAWS_ACCESS_KEY_ID=` + s["akia"] + `\naws_secret_access_key=` + s["awssecret"] + `\nSLACK_BOT_TOKEN=` + s["slack"] + `\n`
	envDouble := strings.ReplaceAll(env, `\n`, `\\n`)
	lines := []string{
		`{"timestamp":"2026-09-01T10:00:00.000Z","type":"session_meta","payload":{"id":"` + uuid + `","timestamp":"2026-09-01T10:00:00Z","cwd":"/Users/jane/work/api","originator":"codex_cli_rs","cli_version":"0.40.0","instructions":"Use token ` + s["ghfg"] + ` for gh.","base_instructions":{"text":"export STRIPE_KEY=` + s["stripe"] + `"},"git":{"commit_hash":"abc","branch":"main","repository_url":"https://jane:` + s["ghp"] + `@github.com/acme/api.git"}}}`,
		`{"timestamp":"2026-09-01T10:00:01.000Z","type":"turn_context","payload":{"cwd":"/Users/jane/work/api","approval_policy":"on-request","sandbox_policy":{"mode":"workspace-write"},"model":"gpt-5-codex","user_instructions":"my key is ` + s["openai"] + `"}}`,
		`{"timestamp":"2026-09-01T10:00:02.000Z","type":"event_msg","payload":{"type":"user_message","message":"use ` + s["anthropic"] + ` please","images":[]}}`,
		`{"timestamp":"2026-09-01T10:00:03.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Authorization: Bearer ` + s["bearer"] + `"}]}}`,
		`{"timestamp":"2026-09-01T10:00:04.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"curl -H 'Authorization: Bearer ` + s["bearer"] + `' https://api.x && export T=` + s["jwt"] + `\"],\"workdir\":\"/Users/jane/work/api\"}","call_id":"call_abc"}}`,
		`{"timestamp":"2026-09-01T10:00:05.000Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"call_abc","stdout":"` + env + `","stderr":"","aggregated_output":"` + env + `","exit_code":0,"duration":{"secs":0,"nanos":1},"formatted_output":"` + env + `"}}`,
		`{"timestamp":"2026-09-01T10:00:06.000Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_abc","output":"{\"output\":\"` + envDouble + `\",\"metadata\":{\"exit_code\":0,\"duration_seconds\":0.1}}"}}`,
		`{"timestamp":"2026-09-01T10:00:07.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_def","name":"apply_patch","input":"*** Begin Patch\n*** Add File: .env\n+OPENAI_API_KEY=` + s["openai"] + `\n+JWT=` + s["jwt"] + `\n*** End Patch"}}`,
		`{"timestamp":"2026-09-01T10:00:08.000Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"The user key ` + s["stripe"] + ` works"}],"content":null,"encrypted_content":"gAAAAABo` + strings.Repeat("Q", 40) + `"}}`,
		`{"timestamp":"2026-09-01T10:00:09.000Z","type":"event_msg","payload":{"type":"agent_message","message":"I set GITHUB_TOKEN=` + s["ghp"] + `"}}`,
		`{"timestamp":"2026-09-01T10:00:10.000Z","type":"compacted","payload":{"message":"summary with ` + s["akia"] + `","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"key ` + s["openai"] + `"}]}]}}`,
		`{"timestamp":"2026-09-01T10:00:11.000Z","type":"response_item","payload":{"type":"web_search_call","status":"completed","action":{"type":"search","query":"is ` + s["slack"] + ` valid"}}}`,
		`{"timestamp":"2026-09-01T10:00:12.000Z","type":"response_item","payload":{"type":"ghost_snapshot","ghost_commit":{"id":"deadbeef","parent":null,"preexisting_untracked_files":[".env"]}}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func zstdOf(t *testing.T, b []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

type codexFixture struct {
	*fixture
	root string
}

// newCodexFixture resolves the production catalog; the network-bound account source is dropped.
func newCodexFixture(t *testing.T) *codexFixture {
	t.Helper()
	f := &codexFixture{fixture: newFixture(t)}
	f.root = filepath.Join(f.home, ".codex")
	if err := os.MkdirAll(filepath.Join(f.root, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.resolveCatalog(func(s sources.Resolved) bool { return s.Family == "codex" && s.Gather != "account" })
	return f
}

// resolveCatalog points f at the production catalog resolved over its home, keeping the sources keep accepts.
func (f *fixture) resolveCatalog(keep func(sources.Resolved) bool) sources.Env {
	f.t.Helper()
	catalog, err := sources.Load()
	if err != nil {
		f.t.Fatal(err)
	}
	env := sources.Env{Home: f.home, Lookup: func(string) (string, bool) { return "", false }}
	eff, err := config.Resolve(config.Input{Catalog: catalog, Env: env, StateDir: f.stateDir})
	if err != nil {
		f.t.Fatal(err)
	}
	eff.Sources = slices.DeleteFunc(eff.Sources, func(s sources.Resolved) bool { return !keep(s) })
	f.eff = eff
	return env
}

func (f *codexFixture) write(rel string, body []byte) string {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	writeFile(f.t, p, body)
	return p
}

func (f *codexFixture) move(from, toRel string) string {
	f.t.Helper()
	to := filepath.Join(f.root, toRel)
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		f.t.Fatal(err)
	}
	return to
}

// keyFor is the mirror key a logical name ships to under codex-rollouts.
func (f *codexFixture) keyFor(logical string) string {
	f.t.Helper()
	k, err := formats.MirrorKey("default", f.unit.InstallID.String(), "codex-rollouts", f.unit.NameKey,
		formats.CanonicalPath(logical, engine.UsernameFromStateDir(f.stateDir)))
	if err != nil {
		f.t.Fatal(err)
	}
	return k
}

// plaintext opens a stored object; a zstd payload is decoded so a leak inside it is still seen.
func (f *fixture) plaintext(key string) (transforms.Manifest, string) {
	f.t.Helper()
	obj, ok := f.port.get(key)
	if !ok {
		f.t.Fatalf("no object at %s", key)
	}
	m, payload, err := transforms.Open(obj.Body, f.unit.Identity)
	if err != nil {
		f.t.Fatalf("open %s: %v", key, err)
	}
	if bytes.HasPrefix(payload, []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		dec, err := zstd.NewReader(nil)
		if err != nil {
			f.t.Fatal(err)
		}
		defer dec.Close()
		if payload, err = dec.DecodeAll(payload, nil); err != nil {
			f.t.Fatalf("%s: shipped zstd payload does not decode: %v", m.NativePath, err)
		}
	}
	return m, string(payload)
}

func assertNoCodexSecret(t *testing.T, m transforms.Manifest, body string) {
	t.Helper()
	for name, secret := range codexSecrets {
		if n := strings.Count(body, secret); n > 0 {
			t.Errorf("%s credential survives %dx in %s (source %s)", name, n, m.NativePath, m.SourceID)
		}
	}
}

func (f *codexFixture) assertKeys(want ...string) {
	f.t.Helper()
	want = slices.Sorted(slices.Values(want))
	if got := f.port.keys(); !slices.Equal(got, want) {
		f.t.Fatalf("bucket keys\n got: %v\nwant: %v", got, want)
	}
}

func TestCodexCompressedRolloutIsScrubbed(t *testing.T) {
	f := newCodexFixture(t)
	rollout := codexRollout(codexUUID1)
	f.write("sessions/2026/09/20/"+codexRolloutName(codexUUID1), []byte(rollout))
	f.write("archived_sessions/"+codexRolloutName(codexUUID2), []byte(codexRollout(codexUUID2)))
	f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID3)+".zst", zstdOf(t, []byte(codexRollout(codexUUID3))))
	f.write("session_index.jsonl", []byte(`{"id":"`+codexUUID1+`","thread_name":"rotate `+codexSecrets["openai"]+`","updated_at":"2026-09-20T10:00:00Z"}`+"\n"))

	if rep := f.runUnbounded(); rep.Shipped != 4 {
		t.Fatalf("want 4 shipped objects (live, archived, cold, index), got %+v", rep)
	}
	for _, key := range f.port.keys() {
		m, body := f.plaintext(key)
		if !strings.Contains(key, "/source=codex-rollouts/") {
			t.Errorf("%s shipped under %s; every Codex rollout belongs to codex-rollouts", filepath.Base(m.NativePath), m.SourceID)
		}
		assertNoCodexSecret(t, m, body)
	}
}

func TestCodexMirrorKeyIsTheSessionUUID(t *testing.T) {
	f := newCodexFixture(t)
	f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID1), []byte(codexRollout(codexUUID1)))
	f.write("session_index.jsonl", []byte(`{"id":"`+codexUUID1+`","thread_name":"t","updated_at":"2026-09-20T10:00:00Z"}`+"\n"))
	f.run()
	// The index is not a rollout, so it keeps path identity.
	f.assertKeys(f.keyFor("codex-session/"+codexUUID1), f.keyFor("session_index.jsonl"))
}

// Codex moves a finished rollout to archived_sessions/ and later zstd-compresses it: still one
// session, one object, one upload.
func TestCodexRolloutLifecycleIsOneObject(t *testing.T) {
	f := newCodexFixture(t)
	rollout := []byte(codexRollout(codexUUID1))
	live := f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID1), rollout)
	if rep := f.run(); rep.Shipped != 1 {
		t.Fatalf("first tick: want 1 shipped, got %+v", rep)
	}
	f.port.reset()

	archived := f.move(live, "archived_sessions/"+codexRolloutName(codexUUID1))
	idle := func(label string) {
		t.Helper()
		if rep := f.run(); rep.Shipped != 0 || f.port.putCount() != 0 {
			t.Errorf("%s: want no upload, got shipped=%d puts=%d keys=%v", label, rep.Shipped, f.port.putCount(), f.port.keys())
		}
	}
	idle("archived")
	f.write("archived_sessions/"+codexRolloutName(codexUUID1)+".zst", zstdOf(t, rollout))
	if err := os.Remove(archived); err != nil {
		t.Fatal(err)
	}
	idle("compressed")
	idle("idle")

	f.assertKeys(f.keyFor("codex-session/" + codexUUID1))
}

func TestCodexPlainAndCompressedInOneTickShipOnce(t *testing.T) {
	f := newCodexFixture(t)
	rollout := []byte(codexRollout(codexUUID1))
	f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID1), rollout)
	f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID1)+".zst", zstdOf(t, rollout))
	f.run()
	if n := f.port.putCount(); n != 1 {
		t.Errorf("want 1 upload for one session in two forms, got %d", n)
	}
	key := f.keyFor("codex-session/" + codexUUID1)
	f.assertKeys(key)
	// The plaintext form is preferred when both exist.
	if m, _ := f.plaintext(key); strings.HasSuffix(m.NativePath, ".zst") {
		t.Errorf("shipped the compressed form %s while the plaintext one exists", m.NativePath)
	}
}

func TestCodexStateWipeConvergesWithoutNewKeys(t *testing.T) {
	f := newCodexFixture(t)
	rollout := []byte(codexRollout(codexUUID1))
	f.write("archived_sessions/"+codexRolloutName(codexUUID1), rollout)
	f.write("archived_sessions/"+codexRolloutName(codexUUID1)+".zst", zstdOf(t, rollout))
	f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID2)+".zst", zstdOf(t, []byte(codexRollout(codexUUID2))))
	f.run()
	want := []string{f.keyFor("codex-session/" + codexUUID1), f.keyFor("codex-session/" + codexUUID2)}
	f.assertKeys(want...)

	f.wipeState()
	f.run()
	f.assertKeys(want...)
}

func TestCodexContentChangeAfterArchiveOverwritesTheSameKey(t *testing.T) {
	f := newCodexFixture(t)
	live := f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID1), []byte(codexRollout(codexUUID1)))
	f.run()
	archived := f.move(live, "archived_sessions/"+codexRolloutName(codexUUID1))
	fh, err := os.OpenFile(archived, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(codexResumed); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	if rep := f.run(); rep.Shipped != 1 {
		t.Fatalf("changed content must ship, got %+v", rep)
	}
	key := f.keyFor("codex-session/" + codexUUID1)
	f.assertKeys(key)
	if obj, _ := f.port.get(key); obj.Versions != 2 {
		t.Errorf("want the change as a second version of the same key, got %d versions", obj.Versions)
	}
	if _, body := f.plaintext(key); !strings.Contains(body, `"message":"resumed"`) {
		t.Error("the overwrite does not carry the appended line")
	}
}

// Sizes of the plaintext and compressed forms are not comparable: a session that grew and was then
// compressed is not a truncation, while a compressed rollout replaced by a smaller one still is.
func TestCodexCompressionIsNotReportedAsShrinking(t *testing.T) {
	f := newCodexFixture(t)
	rollout := codexRollout(codexUUID1)
	live := f.write("sessions/2026/09/01/"+codexRolloutName(codexUUID1), []byte(rollout))
	f.run()
	f.write("archived_sessions/"+codexRolloutName(codexUUID1)+".zst", zstdOf(t, []byte(rollout+codexResumed)))
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	shipReason := func() string {
		t.Helper()
		rep := f.run()
		if rep.Shipped != 1 {
			t.Fatalf("want 1 shipped, got %+v", rep)
		}
		for _, so := range rep.Sources {
			for _, fo := range so.Files {
				if fo.Decision == auditlog.DecisionShipped {
					return fo.Reason
				}
			}
		}
		return ""
	}
	if reason := shipReason(); strings.Contains(reason, "shrank") {
		t.Errorf("compressing a grown rollout reported %q", reason)
	}

	f.write("archived_sessions/"+codexRolloutName(codexUUID1)+".zst", zstdOf(t, []byte(rollout[:len(rollout)/2])))
	if reason := shipReason(); !strings.Contains(reason, "shrank") {
		t.Errorf("a truncated compressed rollout reported %q, want the shrink signal", reason)
	}
}
