package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// --- harness ----------------------------------------------------------------

// fakeHome builds a home directory with a plausible Claude Code store, so root resolution has something real to act on.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, ".claude", "projects", "-Users-jane-work-api"))
	mustWrite(t, filepath.Join(home, ".claude", "projects", "-Users-jane-work-api", "s.jsonl"), "{}\n")
	mustWrite(t, filepath.Join(home, ".claude", "CLAUDE.md"), "# memory\n")
	return home
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func env(home string, vars map[string]string) sources.Env {
	return sources.Env{
		Home: home,
		Lookup: func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		},
	}
}

func loadCatalog(t *testing.T) *sources.Compiled {
	t.Helper()
	c, err := sources.Load()
	if err != nil {
		t.Fatalf("compiled catalog must load and validate: %v", err)
	}
	return c
}

func doc(t *testing.T, y string) *config.Document {
	t.Helper()
	d, err := config.ParseDocument([]byte(y))
	if err != nil {
		t.Fatalf("parse document: %v", err)
	}
	return d
}

// servedDoc parses the way the served path does: tolerant of fields this build does not know.
func servedDoc(t *testing.T, y string) *config.Document {
	t.Helper()
	d, err := config.ParseServedDocument([]byte(y))
	if err != nil {
		t.Fatalf("parse served document: %v", err)
	}
	return d
}

func baseInput(t *testing.T, home string, layers ...config.LayeredDocument) config.Input {
	t.Helper()
	return config.Input{
		Catalog:  loadCatalog(t),
		Layers:   layers,
		Env:      env(home, nil),
		StateDir: t.TempDir(),
	}
}

// --- baseline ---------------------------------------------------------------

func TestResolveWithNoConfigFilesWorks(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatalf("a clone-and-run install with no config files must resolve: %v", err)
	}

	var claude *config.ResolvedSource
	for i := range eff.Sources {
		if eff.Sources[i].ID == "claude-code-transcripts" {
			claude = &eff.Sources[i]
		}
	}
	if claude == nil {
		t.Fatal("claude-code-transcripts missing from the resolved set")
	}
	if claude.Root != filepath.Join(home, ".claude") {
		t.Errorf("root should resolve to the fake home store, got %q (%s)",
			claude.Root, claude.RootUnresolvedReason)
	}
}

// Every value is attributable to the layer that set it.
func TestProvenanceAttributesEveryValue(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
mode:
  schedule: "5m"
sources:
  - id: cursor-transcripts
    enabled: false
`)},
		config.LayeredDocument{Layer: config.LayerRemote, Doc: servedDoc(t, `
max_files_per_run: 32
`)},
	))
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]config.Layer{
		"max_files_per_run":                       config.LayerRemote,
		"mode.schedule":                           config.LayerUser,
		"sources.cursor-transcripts.enabled":      config.LayerUser,
		"send.sink":                               config.LayerCompiledDefaults,
		"scrub.rule_packs":                        config.LayerCompiledDefaults,
		"sources.claude-code-transcripts.include": config.LayerBundledCatalog,
	}
	for field, layer := range want {
		got, ok := eff.Provenance[field]
		if !ok {
			t.Errorf("%s has no provenance recorded", field)
			continue
		}
		if got.Layer != layer {
			t.Errorf("%s: provenance %s, want %s", field, got.Layer, layer)
		}
		if got.Layer == 0 && !got.Derived {
			t.Errorf("%s: origin names no layer", field)
		}
	}
}

// A user layer disabling a source beats a remote layer enabling it.
func TestLocalDenyBeatsRemoteAllow(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: claude-code-transcripts
    enabled: false
`)},
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
sources:
  - id: claude-code-transcripts
    enabled: true
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range eff.Sources {
		if s.ID == "claude-code-transcripts" && s.Enabled {
			t.Fatal("a remote enable must not undo a local disable")
		}
	}
}

// --- config_version ---------------------------------------------------------

func TestUnknownConfigVersionIsAHardError(t *testing.T) {
	home := fakeHome(t)
	_, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, "config_version: 99\n")},
	))
	if err == nil {
		t.Fatal("an unknown config_version must be a hard error, never a partial application")
	}
	if !strings.Contains(err.Error(), "config_version") {
		t.Errorf("the refusal should name the field, got: %v", err)
	}
}

// --- scope ceiling ----------------------------------------------------------

// A root the compiled catalog never declared is refused: adding a genuinely new one takes a release.
func TestOutsideCeilingRootIsRejected(t *testing.T) {
	home := fakeHome(t)
	mustMkdir(t, filepath.Join(home, "evil", "projects"))

	_, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
sources:
  - id: claude-code-transcripts
    roots: ["~/evil"]
`)},
	))
	if err == nil {
		t.Fatal("a root outside the compiled ceiling must be rejected")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("the refusal should explain the ceiling, got: %v", err)
	}
}

// A user-layer adjustment within an already-compiled root family works with no recompilation.
func TestUserLayerMayNarrowWithinACompiledRoot(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: claude-code-transcripts
    include: ["projects/**/*.jsonl"]
`)},
	))
	if err != nil {
		t.Fatalf("narrowing includes within a compiled root must be allowed: %v", err)
	}
	for _, s := range eff.Sources {
		if s.ID == "claude-code-transcripts" {
			if len(s.Include) != 1 || s.Include[0] != "projects/**/*.jsonl" {
				t.Errorf("include not applied: %v", s.Include)
			}
		}
	}
}

// The control plane's served document in the exact shape renderConfig composes it. This is the
// cross-repo contract: a field here that starts being refused makes the client refuse the WHOLE
// document and fall back to its cached config, so the server must stop serving it first. The
// send: block is the s3 one still served to the pre-vend fleet and has to keep LOADING here.
func TestServedDocumentWithAuthoredPartResolves(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: servedDoc(t, `
issued_at: 2026-08-12T10:00:00Z
org: acme
send:
  sink: s3
  bucket: acme-archive
  region: eu-central-1
encryption:
  additional_recipients:
    - age1rqcz770l8nugps6nxpz8lwgcrcqkcrz982jf6z6mjqlkvx6rgcssz7h2ys
mode:
  schedule: "30m"
sources:
  - id: claude-code-transcripts
    include: ["projects/**/*.jsonl"]
scrub:
  rule_packs: [gitleaks-core]
max_files_per_run: 200
`)},
	))
	if err != nil {
		t.Fatalf("the served document must resolve: %v", err)
	}
	if eff.Schedule != "30m" {
		t.Errorf("mode.schedule = %q: the org's schedule did not take effect", eff.Schedule)
	}
	if eff.MaxFilesPerRun != 200 {
		t.Errorf("max_files_per_run = %d, want 200", eff.MaxFilesPerRun)
	}
	if got := eff.Provenance["mode.schedule"]; got.Layer != config.LayerRemote {
		t.Errorf("mode.schedule provenance %s, want the remote layer", got.Layer)
	}
}

// --- the deny list ----------------------------------------------------------

// A config whose include glob reaches a compiled-deny path is rejected and reported, not applied.
func TestIncludeReachingAgentCredentialsIsRejected(t *testing.T) {
	home := fakeHome(t)
	mustWrite(t, filepath.Join(home, ".claude", ".credentials.json"), `{"accessToken":"secret"}`)

	_, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
sources:
  - id: claude-code-transcripts
    include: ["**"]
`)},
	))
	if err == nil {
		t.Fatal("an include glob reaching .credentials.json must be rejected")
	}
	if !strings.Contains(err.Error(), "deny") {
		t.Errorf("the refusal should name the deny list, got: %v", err)
	}
}

func TestDenyListMatchesCredentialShapes(t *testing.T) {
	home := t.TempDir()
	d := sources.New(home)

	denied := []string{
		filepath.Join(home, ".aws", "credentials"),
		filepath.Join(home, ".ssh", "id_rsa"),
		filepath.Join(home, ".ssh", "nested", "deeper", "key"),
		filepath.Join(home, ".claude", ".credentials.json"),
		filepath.Join(home, ".claude.json"),
		filepath.Join(home, ".codex", "auth.json"),
		filepath.Join(home, ".netrc"),
		filepath.Join(home, "work", "project", ".env"),
		filepath.Join(home, "work", "project", ".env.local"),
		filepath.Join(home, "work", "certs", "server.pem"),
		filepath.Join(home, "Library", "Keychains", "login.keychain-db"),
		filepath.Join(home, ".config", "gh", "hosts.yml"),
	}
	for _, p := range denied {
		if ok, _ := d.Match(p); !ok {
			t.Errorf("must be denied: %s", p)
		}
	}

	allowed := []string{
		filepath.Join(home, ".claude", "projects", "proj", "s.jsonl"),
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(home, ".codex", "sessions", "2026", "r.jsonl"),
		filepath.Join(home, "work", "project", "src", "db.go"),
	}
	for _, p := range allowed {
		if ok, pat := d.Match(p); ok {
			t.Errorf("must not be denied: %s (matched %q)", p, pat)
		}
	}
}

// The deny list applies to the resolved path, so a symlink under an allowed root cannot launder a denied target.
func TestDenyFollowsSymlinksToTheirTarget(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(home, ".ssh", "id_ed25519")
	mustWrite(t, secret, "PRIVATE KEY")

	link := filepath.Join(home, ".claude", "projects", "innocent.jsonl")
	mustMkdir(t, filepath.Dir(link))
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	d := sources.New(home)
	if ok, _ := d.Match(link); !ok {
		t.Fatal("a symlink whose target is denied must itself be denied")
	}
}

// --- require_subdir ---------------------------------------------------------

// A root that does not look like the store it claims to be is not that store.
func TestRequireSubdirRefusesAWrongShapedRoot(t *testing.T) {
	home := t.TempDir()
	mustMkdir(t, filepath.Join(home, ".claude"))
	// No projects/ dir: the root exists but is the wrong shape.

	eff, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range eff.Sources {
		if s.ID == "claude-code-transcripts" {
			if s.Root != "" {
				t.Errorf("a root with no projects/ dir must not resolve, got %q", s.Root)
			}
			if !strings.Contains(s.RootUnresolvedReason, "projects") {
				t.Errorf("the reason should name the missing subdir, got %q", s.RootUnresolvedReason)
			}
		}
	}
}

// --- env expansion ----------------------------------------------------------

// Expansion is an attack surface: the deny list must act on the expanded value, never the template.
func TestEnvVarRootIsExpandedThenDenyChecked(t *testing.T) {
	home := fakeHome(t)
	in := baseInput(t, home)
	in.Env = env(home, map[string]string{"CLAUDE_CONFIG_DIR": filepath.Join(home, ".ssh")})

	_, err := config.Resolve(in)
	if err == nil {
		t.Fatal("a root whose env var points into ~/.ssh must be rejected after expansion")
	}
	if !strings.Contains(err.Error(), "deny") {
		t.Errorf("the refusal should name the deny list, got: %v", err)
	}
}

// An unset variable is the normal case, not an error: the next candidate is tried.
func TestUnsetEnvVarFallsThroughToTheNextRoot(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range eff.Sources {
		if s.ID == "claude-code-transcripts" && s.Root == "" {
			t.Errorf("with $CLAUDE_CONFIG_DIR unset, ~/.claude must still resolve: %s", s.RootUnresolvedReason)
		}
	}
}

func TestRelativeExpansionIsRefused(t *testing.T) {
	home := fakeHome(t)
	e := env(home, map[string]string{"CLAUDE_CONFIG_DIR": "relative/path"})
	if _, err := e.ExpandRoot("$CLAUDE_CONFIG_DIR"); err == nil {
		t.Fatal("a root expanding to a relative path must be refused")
	}
}

// --- authority enforcement --------------------------------------------------

// A layer's exemptions MERGE with the compiled baseline rather than replacing it; detection rules are free to add.
func TestServedExemptionsMergeWithTheCompiledBaseline(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
structural_exempt:
  claude-code: [acmeInternalTraceId]
`)},
	))
	if err != nil {
		t.Fatalf("the served layer may carry exemptions: %v", err)
	}
	if !slices.Contains(eff.StructuralEx["claude-code"], "acmeInternalTraceId") {
		t.Errorf("served exemption not applied: %v", eff.StructuralEx)
	}
	for _, compiled := range transforms.CompiledExemptions()["claude-code"] {
		if !slices.Contains(eff.StructuralEx["claude-code"], compiled) {
			t.Errorf("served addition displaced compiled exemption %q", compiled)
		}
	}
}

// A clone-and-run install must come up with the compiled exemption baseline already in force.
// Asserted by name so a future trim of CompiledExemptions cannot silently drop a join key
// and keep this test green.
func TestCompiledExemptionBaselineIsSeededByDefault(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"toolUseId", "message.content[].id", "message.content[].tool_use_id"} {
		if !slices.Contains(eff.StructuralEx["claude-code"], p) {
			t.Errorf("claude-code join key %q not exempt by default", p)
		}
	}
}

// Union, not replace: a layer naming one real pack must not drop the four the defaults carry.
func TestRulePacksUnionRatherThanReplace(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
scrub:
  rule_packs: [pii-core]
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gitleaks-core", "cloud-keys", "generic-entropy", "pii-core"} {
		found := false
		for _, got := range eff.RulePacks {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("rule pack %q missing from %v", want, eff.RulePacks)
		}
	}
}

// The served config names the organization; it cannot name the write path, so the send block it serves is ignored.
func TestServedRemoteConfigSetsOrgAndIgnoresTheSendBlock(t *testing.T) {
	home := fakeHome(t)
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: servedDoc(t, `
org: acme
send:
  sink: s3
  bucket: acme-archive
  region: eu-central-1
`)},
	))
	if err != nil {
		t.Fatalf("the served config must resolve: %v", err)
	}
	if eff.OrganizationID != "acme" {
		t.Errorf("served config not applied: %s", eff.OrganizationID)
	}
}

// Envelope fields are refused from any layer but the remote one: a local org would relabel where this machine writes.
func TestEnvelopeFieldsAreRefusedFromLocalLayers(t *testing.T) {
	home := fakeHome(t)
	for _, y := range []string{"org: acme\n", "issued_at: 2026-01-01T00:00:00Z\n"} {
		_, err := config.Resolve(baseInput(t, home,
			config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, y)},
		))
		var rej *config.RejectionError
		if !errors.As(err, &rej) {
			t.Errorf("a user layer set %q and it was accepted: %v", strings.TrimSpace(y), err)
		}
	}
}

// Standalone writes organization=default; an enterprise install takes its organization from the served config.
func TestOrganizationComesFromServedConfigNotAConstant(t *testing.T) {
	home := fakeHome(t)

	standalone, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatal(err)
	}
	if standalone.OrganizationID != "default" {
		t.Errorf("standalone organization %q, want default", standalone.OrganizationID)
	}

	enterprise, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, "org: acme\n")},
	))
	if err != nil {
		t.Fatal(err)
	}
	if enterprise.OrganizationID != "acme" {
		t.Errorf("enterprise organization %q, want acme", enterprise.OrganizationID)
	}
}

// The two parse paths on one document: a served document may carry fields this build does not know, the local file may not.
func TestServedParseToleratesUnknownFieldsAndTheLocalParseDoesNot(t *testing.T) {
	home := fakeHome(t)
	const body = "config_version: 1\n" +
		"send:\n  sink: s3\n  bucket: acme-archive\n" +
		"future_top_level: 7\n" +
		"mode:\n  schedule: \"5m\"\n  future_nested: yes\n"

	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: servedDoc(t, body)},
	))
	if err != nil {
		t.Fatalf("a served document with unknown fields must resolve: %v", err)
	}
	if eff.Schedule != "5m" {
		t.Errorf("the fields this build does know must still apply: schedule %q", eff.Schedule)
	}

	if _, err := config.ParseDocument([]byte(body)); err == nil {
		t.Fatal("the strict local parse accepted a field this build does not know")
	}
}

// --- the spec fingerprint gate ----------------------------------------------

// A config push touching only a redaction rule and the run budget must invalidate no fingerprints.
func TestSpecFingerprintCoversOnlyReadAffectingFields(t *testing.T) {
	home := fakeHome(t)

	before, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatal(err)
	}

	// A push that changes a redaction rule and the run budget.
	unrelated, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
scrub:
  rule_packs: [pii-core]
max_files_per_run: 8
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	for i := range before.Sources {
		if before.Sources[i].SpecFingerprint != unrelated.Sources[i].SpecFingerprint {
			t.Errorf("%s: a redaction/budget push must not invalidate fingerprints",
				before.Sources[i].ID)
		}
	}

	// A glob change resets exactly one source.
	globbed, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: claude-code-transcripts
    include: ["projects/**/*.jsonl"]
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for i := range before.Sources {
		if before.Sources[i].SpecFingerprint != globbed.Sources[i].SpecFingerprint {
			changed++
			if before.Sources[i].ID != "claude-code-transcripts" {
				t.Errorf("%s: an unrelated source's fingerprint changed", before.Sources[i].ID)
			}
		}
	}
	if changed != 1 {
		t.Errorf("a glob change should reset exactly one source, reset %d", changed)
	}
}

// --- documents --------------------------------------------------------------

// A typo in a security-relevant config file must not be a silent no-op.
func TestUnknownFieldInADocumentIsRefused(t *testing.T) {
	if _, err := config.ParseDocument([]byte("max_file_per_run: 8\n")); err == nil {
		t.Fatal("an unknown config field must be refused, not ignored")
	}
}

func TestSourceOverrideForUnknownSourceIsRefused(t *testing.T) {
	home := fakeHome(t)
	_, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: not-a-real-source
    enabled: true
`)},
	))
	if err == nil {
		t.Fatal("a config layer must not be able to invent a source")
	}
}

// A root that does not exist must not resolve: agent_absent has to stay separable from root_present_no_match.
func TestNonExistentRootDoesNotResolve(t *testing.T) {
	home := fakeHome(t) // has ~/.claude, deliberately no ~/.cursor

	eff, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range eff.Sources {
		if s.Family != "cursor" {
			continue
		}
		if s.Root != "" {
			t.Errorf("%s: root %q resolved although the directory does not exist", s.ID, s.Root)
		}
		if !strings.Contains(s.RootUnresolvedReason, "does not exist") {
			t.Errorf("%s: reason should say the root is absent, got %q", s.ID, s.RootUnresolvedReason)
		}
	}
}

// A file where a root directory is expected is not a store either.
func TestRootThatIsAFileDoesNotResolve(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".claude"), "not a directory")

	eff, err := config.Resolve(baseInput(t, home))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range eff.Sources {
		if s.Family == "claude-code" && s.Root != "" {
			t.Errorf("%s: a regular file must not resolve as a root, got %q", s.ID, s.Root)
		}
	}
}

// --- M8: the state directory and the drain deadline -------------------------

// state_dir is machine-owner only: a served config that could move it would silently defeat a pause.
func TestTheServedDocumentCannotMoveTheStateDirectory(t *testing.T) {
	home := fakeHome(t)
	_, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote,
			Doc: doc(t, "state_dir: /tmp/somewhere-else\n")},
	))
	var rej *config.RejectionError
	if !errors.As(err, &rej) {
		t.Fatalf("the served document moved the state directory: %v", err)
	}
	if rej.Field != "state_dir" {
		t.Errorf("rejected the wrong field: %s", rej.Field)
	}
}

func TestTheUserLayerMayMoveTheStateDirectory(t *testing.T) {
	home := fakeHome(t)
	// The machine owner's own file must still set it, or the field is settable by nobody.
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "state_dir: /tmp/mine\n")},
	))
	if err != nil {
		t.Fatal(err)
	}
	if eff.StateDir != "/tmp/mine" {
		t.Errorf("state_dir = %q", eff.StateDir)
	}
}

func TestDrainDeadlineParses(t *testing.T) {
	home := fakeHome(t)

	// Distinct from the default, so the assertion can only pass by parsing the document.
	eff, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "drain_deadline: 90s\n")},
	))
	if err != nil {
		t.Fatal(err)
	}
	if eff.DrainDeadline != 90*time.Second {
		t.Errorf("drain_deadline = %s, want 90s", eff.DrainDeadline)
	}
}

func TestANonPositiveDrainDeadlineIsRejected(t *testing.T) {
	home := fakeHome(t)
	for _, spelling := range []string{"0s", "-30s"} {
		// A zero or negative deadline makes every drain a silent no-op, losing the data the drain exists to save.
		_, err := config.Resolve(baseInput(t, home,
			config.LayeredDocument{Layer: config.LayerUser,
				Doc: doc(t, "drain_deadline: "+spelling+"\n")},
		))
		var rej *config.RejectionError
		if !errors.As(err, &rej) {
			t.Errorf("drain_deadline %s was accepted: %v", spelling, err)
		}
	}
}

func TestAnUnparseableDrainDeadlineIsRejected(t *testing.T) {
	home := fakeHome(t)
	_, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "drain_deadline: 60\n")},
	))
	// Bare "60" is the plausible mistake, and guessing at seconds would mean a config that means something else.
	var rej *config.RejectionError
	if !errors.As(err, &rej) {
		t.Fatalf("drain_deadline: 60 was accepted: %v", err)
	}
}

// --- the enricher setting ---------------------------------------------------

func TestALocalLayerCanDisableAndEnableARegisteredEnricher(t *testing.T) {
	home := fakeHome(t)

	off, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: cursor-transcripts
    enrichers:
      cursor-transcript-join: false
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	if enrichersOf(off, "cursor-transcripts")["cursor-transcript-join"] {
		t.Error("a local disable did not take effect")
	}

	// And back on: enabling has to work from a local layer or the switch is one-way.
	on, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: cursor-transcripts
    enrichers:
      cursor-transcript-join: true
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	if !enrichersOf(on, "cursor-transcripts")["cursor-transcript-join"] {
		t.Error("a local enable did not take effect")
	}
}

// Toggling must not edit the compiled catalog. The two resolutions deliberately SHARE one
// *sources.Compiled: against two freshly loaded catalogs this would pass whether or not the clone exists.
func TestTogglingAnEnricherDoesNotMutateTheCompiledCatalog(t *testing.T) {
	home := fakeHome(t)
	shared := loadCatalog(t)

	first := config.Input{
		Catalog: shared,
		Layers: []config.LayeredDocument{{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: cursor-transcripts
    enrichers:
      cursor-transcript-join: false
`)}},
		Env:      env(home, nil),
		StateDir: t.TempDir(),
	}
	if _, err := config.Resolve(first); err != nil {
		t.Fatal(err)
	}

	// The same catalog, no override: if the first resolution wrote through, this still sees the enricher disabled.
	clean, err := config.Resolve(config.Input{
		Catalog:  shared,
		Env:      env(home, nil),
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !enrichersOf(clean, "cursor-transcripts")["cursor-transcript-join"] {
		t.Error("a previous resolution's override leaked into the compiled catalog")
	}
}

func TestARemoteLayerMayDisableButNotEnableAnEnricher(t *testing.T) {
	home := fakeHome(t)

	// Disabling from the served document is fine: it only ever narrows.
	off, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
sources:
  - id: cursor-transcripts
    enrichers:
      cursor-transcript-join: false
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	if enrichersOf(off, "cursor-transcripts")["cursor-transcript-join"] {
		t.Error("a remote layer could not disable an enricher, but narrowing is always allowed")
	}

	// Enabling from a non-local layer would widen what gets read: an enricher reads a database the raw pipeline never touches.
	on, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: cursor-transcripts
    enrichers:
      cursor-transcript-join: false
`)},
		config.LayeredDocument{Layer: config.LayerRemote, Doc: doc(t, `
sources:
  - id: cursor-transcripts
    enrichers:
      cursor-transcript-join: true
`)},
	))
	if err != nil {
		t.Fatal(err)
	}
	if enrichersOf(on, "cursor-transcripts")["cursor-transcript-join"] {
		t.Error("a remote layer enabled an enricher the machine owner had switched off")
	}
}

func TestALayerCannotAttachAnEnricherTheCatalogDidNot(t *testing.T) {
	home := fakeHome(t)
	_, err := config.Resolve(baseInput(t, home,
		config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, `
sources:
  - id: claude-code-transcripts
    enrichers:
      cursor-transcript-join: true
`)},
	))
	// Attaching an enricher the catalog never gave this source would let config decide which code reads which store.
	var rej *config.RejectionError
	if !errors.As(err, &rej) {
		t.Fatalf("a layer attached an enricher the catalog did not: %v", err)
	}
}

func enrichersOf(eff *config.Effective, id string) map[string]bool {
	for _, s := range eff.Sources {
		if s.ID == id {
			return s.Enrichers
		}
	}
	return nil
}
