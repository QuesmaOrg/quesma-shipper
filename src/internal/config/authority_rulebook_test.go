package config_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	protocol "github.com/QuesmaOrg/shipper-protocol"
)

// The authority rulebook in the shipper-protocol module bounds a served config's authority; this file makes it executable, and a row without a probe fails.

type rulebookField struct {
	Field     string `json:"field"`
	Default   string `json:"default"`
	ServerMay string `json:"server_may"`
	Note      string `json:"note"`
}

type rulebookDoc struct {
	Comment     string            `json:"comment"`
	Classes     map[string]string `json:"classes"`
	Fields      []rulebookField   `json:"fields"`
	NotSettable []string          `json:"not_settable"`
}

// rulebookProbe exercises one row: accept must apply, reject must be refused, custom covers the asymmetries.
type rulebookProbe struct {
	accept string
	verify func(*testing.T, *config.Effective)
	reject string
	custom func(*testing.T)
}

const (
	probeRecipientA = "age1cpx4grz9j4fkn36cfurggwcg4l0da5fyqadl8fwagtcwy55gt44qlclfa5"
	probeRecipientB = "age14lz0mm7uh45zqavz3pxe89dauhn2e3p7mlu6ve0gt8zhrz2j3ahsz8kf7p"
)

func resolveLayers(t *testing.T, layers ...config.LayeredDocument) (*config.Effective, error) {
	t.Helper()
	return config.Resolve(baseInput(t, fakeHome(t), layers...))
}

func served(t *testing.T, y string) config.LayeredDocument {
	t.Helper()
	return config.LayeredDocument{Layer: config.LayerRemote, Doc: servedDoc(t, y)}
}

func mustReject(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: resolved cleanly, want a rejection", context)
		return
	}
	var rej *config.RejectionError
	if !errors.As(err, &rej) {
		t.Fatalf("%s: failed with %v, want a *RejectionError", context, err)
	}
}

func sourceByID(t *testing.T, eff *config.Effective, id string) *config.ResolvedSource {
	t.Helper()
	for i := range eff.Sources {
		if eff.Sources[i].ID == id {
			return &eff.Sources[i]
		}
	}
	t.Fatalf("source %s missing from the resolved set", id)
	return nil
}

func rulebookProbes(t *testing.T) map[string]rulebookProbe {
	return map[string]rulebookProbe{
		"issued_at / org": {custom: func(t *testing.T) {
			envelope := "issued_at: \"2026-08-12T00:00:00Z\"\norg: acme\n"
			eff, err := resolveLayers(t, served(t, envelope))
			if err != nil {
				t.Fatalf("served envelope refused: %v", err)
			}
			if eff.OrganizationID != "acme" {
				t.Errorf("served org did not become the organization id: %q", eff.OrganizationID)
			}
			_, err = resolveLayers(t, config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, envelope)})
			mustReject(t, err, "envelope in a local file")
		}},

		"config_version": {
			accept: "config_version: 1\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if eff.ConfigVersion != 1 {
					t.Errorf("config_version %d, want 1", eff.ConfigVersion)
				}
				if got := eff.Provenance["config_version"]; got.Layer != config.LayerRemote {
					t.Errorf("served config_version attributed to %s, want the remote layer", got.Layer)
				}
			},
			reject: "config_version: 99\n",
		},

		"mode.schedule": {
			accept: "mode:\n  schedule: \"5m\"\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if eff.Schedule != "5m" {
					t.Errorf("schedule %q", eff.Schedule)
				}
			},
		},

		"max_files_per_run": {
			accept: "max_files_per_run: 32\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if eff.MaxFilesPerRun != 32 {
					t.Errorf("max_files_per_run %d", eff.MaxFilesPerRun)
				}
			},
		},

		"drain_deadline": {
			accept: "drain_deadline: 2h\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if eff.DrainDeadline != 2*time.Hour {
					t.Errorf("drain_deadline %v", eff.DrainDeadline)
				}
			},
			reject: "drain_deadline: -5m\n",
		},

		"state_dir": {
			reject: "state_dir: /var/lib/shipper\n",
		},

		"upload_targets": {
			reject: "upload_targets:\n  - origin: https://evil.example.com\n    addressing: virtual-hosted\n",
			custom: func(t *testing.T) {
				// The machine owner's own layer is the one that may name a destination.
				eff, err := resolveLayers(t, config.LayeredDocument{Layer: config.LayerUser,
					Doc: doc(t, "upload_targets:\n  - origin: https://acme.s3.example.com\n    addressing: virtual-hosted\n")})
				if err != nil {
					t.Fatalf("local upload_targets refused: %v", err)
				}
				if len(eff.UploadTargets) != 1 || eff.UploadTargets[0].Origin != "https://acme.s3.example.com" {
					t.Fatalf("local allowlist did not apply: %+v", eff.UploadTargets)
				}
				if got := eff.Provenance["upload_targets"]; got.Layer != config.LayerUser {
					t.Errorf("upload_targets attributed to %s, want the user layer", got.Layer)
				}
			},
		},

		// The probe uses the s3 document a pre-vend control plane serves: one served document has to work for both fleets.
		"send.sink / bucket / prefix / region / path": {
			accept: "send:\n  sink: s3\n  bucket: acme-archive\n  prefix: teams/acme\n  region: eu-central-1\n  path: /var/tmp/out\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if got := eff.Provenance["send.sink"]; got.Layer != config.LayerCompiledDefaults {
					t.Errorf("an ignored field was attributed to %s: the write path is compiled in", got.Layer)
				}
			},
		},

		"scrub.rule_packs": {custom: func(t *testing.T) {
			// A served subset must not remove the compiled floor: union only adds.
			eff, err := resolveLayers(t, served(t, "scrub:\n  rule_packs:\n    - gitleaks-core\n"))
			if err != nil {
				t.Fatal(err)
			}
			for _, floor := range []string{"gitleaks-core", "quesma-extra", "cloud-keys", "generic-entropy", "pii-core"} {
				if !slices.Contains(eff.RulePacks, floor) {
					t.Errorf("served rule_packs removed compiled pack %s: union must only add", floor)
				}
			}
			_, err = resolveLayers(t, served(t, "scrub:\n  rule_packs:\n    - no-such-pack\n"))
			mustReject(t, err, "a rule pack this build does not have")
		}},

		"scrub.secret_key_names": {custom: func(t *testing.T) {
			eff, err := resolveLayers(t,
				config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "scrub:\n  secret_key_names:\n    - HOUSE_SIG\n")},
				served(t, "scrub:\n  secret_key_names:\n    - ACME_DEPLOY_TOKEN\n"),
			)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"HOUSE_SIG", "ACME_DEPLOY_TOKEN"} {
				if !slices.Contains(eff.SecretKeyNames, name) {
					t.Errorf("union lost %s: %v", name, eff.SecretKeyNames)
				}
			}
		}},

		"structural_exempt": {
			accept: "structural_exempt:\n  claude-jsonl:\n    - message.id\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if !slices.Contains(eff.StructuralEx["claude-jsonl"], "message.id") {
					t.Error("served exemption did not merge")
				}
				for k := range transforms.CompiledExemptions() {
					if _, ok := eff.StructuralEx[k]; !ok {
						t.Errorf("served exemptions replaced the compiled baseline: %s gone", k)
					}
				}
			},
		},

		"encryption.additional_recipients": {custom: func(t *testing.T) {
			eff, err := resolveLayers(t,
				config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "encryption:\n  additional_recipients:\n    - "+probeRecipientA+"\n")},
				served(t, "encryption:\n  additional_recipients:\n    - "+probeRecipientB+"\n"),
			)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range []string{probeRecipientA, probeRecipientB} {
				if !slices.Contains(eff.AdditionalRecipients, r) {
					t.Errorf("union lost recipient %s", r)
				}
			}
		}},

		"encryption.include_install_recipient": {
			accept: "encryption:\n  include_install_recipient: false\n  additional_recipients:\n    - " + probeRecipientA + "\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if eff.IncludeInstallRecipient {
					t.Error("served withhold did not apply")
				}
			},
			// Withhold with no reader would seal objects no key can open.
			reject: "encryption:\n  include_install_recipient: false\n",
		},

		"sources[].enabled": {
			accept: "sources:\n  - id: claude-code-transcripts\n    enabled: false\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if sourceByID(t, eff, "claude-code-transcripts").Enabled {
					t.Error("served disable did not apply")
				}
			},
			// The asymmetry on this field is pinned by TestLocalDenyBeatsRemoteAllow in resolve_test.go.
		},

		"sources[].roots / include / exclude": {
			accept: "sources:\n  - id: claude-code-transcripts\n    roots: [\"~/.claude\"]\n    include: [\"projects/**/*.jsonl\"]\n    exclude: [\"projects/**/tmp/*\"]\n",
			verify: func(t *testing.T, eff *config.Effective) {
				src := sourceByID(t, eff, "claude-code-transcripts")
				if filepath.Base(src.Root) != ".claude" {
					t.Errorf("root did not resolve within the served candidate: %q (%s)", src.Root, src.RootUnresolvedReason)
				}
				if !slices.Equal(src.Include, []string{"projects/**/*.jsonl"}) {
					t.Errorf("include %v", src.Include)
				}
			},
			reject: "sources:\n  - id: claude-code-transcripts\n    roots: [\"~/elsewhere\"]\n",
		},

		"sources[].max_file_bytes": {
			accept: "sources:\n  - id: claude-code-transcripts\n    max_file_bytes: 1024\n",
			verify: func(t *testing.T, eff *config.Effective) {
				src := sourceByID(t, eff, "claude-code-transcripts")
				if src.MaxFileBytes != 1024 {
					t.Errorf("max_file_bytes %d", src.MaxFileBytes)
				}
			},
		},

		"sources[].enrichers{}": {custom: func(t *testing.T) {
			// Disable applies from the served document.
			eff, err := resolveLayers(t, served(t, "sources:\n  - id: cursor-transcripts\n    enrichers:\n      cursor-transcript-join: false\n"))
			if err != nil {
				t.Fatal(err)
			}
			if sourceByID(t, eff, "cursor-transcripts").Enrichers["cursor-transcript-join"] {
				t.Error("served enricher disable did not apply")
			}
			// A served enable over a local disable is ignored, not honored.
			eff, err = resolveLayers(t,
				config.LayeredDocument{Layer: config.LayerUser, Doc: doc(t, "sources:\n  - id: cursor-transcripts\n    enrichers:\n      cursor-transcript-join: false\n")},
				served(t, "sources:\n  - id: cursor-transcripts\n    enrichers:\n      cursor-transcript-join: true\n"),
			)
			if err != nil {
				t.Fatal(err)
			}
			if sourceByID(t, eff, "cursor-transcripts").Enrichers["cursor-transcript-join"] {
				t.Error("served enable overrode a local enricher disable")
			}
			// Attaching an enricher the catalog does not know is refused, not ignored.
			_, err = resolveLayers(t, served(t, "sources:\n  - id: cursor-transcripts\n    enrichers:\n      exfiltrate-everything: true\n"))
			mustReject(t, err, "attaching an unregistered enricher")
		}},

		"a source id not in the compiled catalog": {
			reject: "sources:\n  - id: no-such-source\n    enabled: false\n",
		},

		// The probe uses the document a pre-removal control plane serves: older clients still honor the disable.
		"crash_report.enabled / dsn": {
			accept: "crash_report:\n  enabled: false\n  dsn: https://key@example.com/1\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if _, set := eff.Provenance["crash_report.enabled"]; set {
					t.Error("an ignored field was attributed: crash reporting is removed")
				}
			},
		},

		"autoupdate.enabled": {
			accept: "autoupdate:\n  enabled: false\n",
			verify: func(t *testing.T, eff *config.Effective) {
				if eff.AutoupdateEnabled {
					t.Error("served autoupdate disable did not apply")
				}
			},
			reject: "autoupdate:\n  enabled: true\n",
		},
	}
}

func loadRulebook(t *testing.T) rulebookDoc {
	t.Helper()
	raw, err := fs.ReadFile(protocol.FS, "authority.json")
	if err != nil {
		t.Fatalf("read authority.json: %v", err)
	}
	var rb rulebookDoc
	if err := json.Unmarshal(raw, &rb); err != nil {
		t.Fatalf("parse authority.json: %v", err)
	}
	return rb
}

func TestRulebookMatchesResolver(t *testing.T) {
	rb := loadRulebook(t)
	probes := rulebookProbes(t)

	documented := map[string]bool{}
	for _, f := range rb.Fields {
		documented[f.Field] = true
	}
	for name := range probes {
		if !documented[name] {
			t.Errorf("probe %q has no row in the shipper-protocol authority rulebook", name)
		}
	}

	for _, f := range rb.Fields {
		probe, ok := probes[f.Field]
		if !ok {
			t.Errorf("rulebook row %q has no probe: the row is documentation, not contract", f.Field)
			continue
		}
		if _, known := rb.Classes[f.ServerMay]; !known {
			t.Errorf("row %q uses undeclared class %q", f.Field, f.ServerMay)
			continue
		}
		t.Run(f.Field, func(t *testing.T) {
			// The class dictates the probe's shape, so a row reclassified in the JSON without a behavior change fails here.
			switch f.ServerMay {
			case "refused":
				if probe.accept != "" || probe.reject == "" && probe.custom == nil {
					t.Fatalf("a refused row needs a reject probe and no accept probe")
				}
			case "set", "union":
				if probe.accept == "" && probe.custom == nil {
					t.Fatalf("a %s row needs an accept probe", f.ServerMay)
				}
			case "off-only":
				if (probe.accept == "" || probe.reject == "") && probe.custom == nil {
					t.Fatalf("an off-only row needs both an accept (disable) and a reject (enable) probe")
				}
			case "envelope":
				if probe.custom == nil {
					t.Fatalf("the envelope row needs a custom probe")
				}
			case "ignored":
				// An ignored row is a tolerance claim: the document must LOAD and the field must change nothing.
				if probe.accept == "" || probe.reject != "" {
					t.Fatalf("an ignored row needs an accept probe and no reject probe")
				}
			}
			if probe.accept != "" {
				eff, err := resolveLayers(t, served(t, probe.accept))
				if err != nil {
					t.Fatalf("the served document must be allowed to touch this field: %v", err)
				}
				if probe.verify != nil {
					probe.verify(t, eff)
				}
			}
			if probe.reject != "" {
				_, err := resolveLayers(t, served(t, probe.reject))
				mustReject(t, err, "served document")
			}
			if probe.custom != nil {
				probe.custom(t)
			}
		})
	}
}
