package e2e

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Goldens for the fields where the exact value is the point. What is pinned stays narrow, because a
// golden nobody reads is worse than none: the object key is pinned and can be, being an HMAC over
// the home-relative path, while the source digest is not, being taken before redaction over bytes
// carrying the OS username. Only collected transcripts are recorded; the sidecar and heartbeat would
// pin the environment rather than the behaviour.
//
// A golden diff is a claim that the output SHOULD have changed, and -update on a red test is how a
// regression becomes a committed expectation. Read the diff before running `go test ./e2e -update`.
var update = flag.Bool("update", false, "rewrite the golden files from this run")

// The recorded layout of one object, in reading order.
type goldenObject struct {
	Key           string `json:"key"`
	SourceID      string `json:"source_id"`
	SourceFamily  string `json:"source_family,omitempty"`
	ArtifactClass string `json:"artifact_class"`
	Gather        string `json:"gather"`
	NativePath    string `json:"native_path"`
	ShapeSniff    string `json:"shape_sniff,omitempty"`

	SourceHash   string `json:"source_hash"`
	ShippedHash  string `json:"shipped_hash"`
	PayloadSize  int64  `json:"payload_size"`
	PayloadLines int    `json:"payload_lines"`
	PayloadMTime string `json:"payload_mtime,omitempty"`

	Redaction *goldenRedaction `json:"redaction,omitempty"`

	Derived          bool     `json:"derived,omitempty"`
	Enricher         string   `json:"enricher,omitempty"`
	DerivedFrom      []string `json:"derived_from,omitempty"`
	EnrichStatus     string   `json:"enrich_status,omitempty"`
	EnrichMismatches int      `json:"enrich_mismatches,omitempty"`

	// enrich_status stays "ok" for the explained shortfalls, so without these an object short
	// of some enrichment is indistinguishable here from one carrying all of it.
	EnrichRepeats          int `json:"enrich_repeats,omitempty"`
	EnrichTail             int `json:"enrich_tail,omitempty"`
	EnrichAmbiguous        int `json:"enrich_ambiguous,omitempty"`
	EnrichLineDecodeErrors int `json:"enrich_line_decode_errors,omitempty"`

	Recipients  []string `json:"recipients,omitempty"`
	PayloadFile string   `json:"payload_file"`
}

// Counts, not density: a ratio over the whole file moves whenever a fixture gains a character.
type goldenRedaction struct {
	Rules map[string]int `json:"rules,omitempty"`
}

func TestGolden(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test exercises UNIX paths, not available on Windows")
	}
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, w *world, username string)
	}{
		{
			name: "claude-2026-07",
			stage: func(t *testing.T, w *world, username string) {
				stageClaude(t, w, username)
			},
		},
		{
			// On the record as a finding, not an endorsement: a session id with no hex letters is
			// digits and dashes, which the card-pan rule eats, destroying the join key while the
			// object still ships looking fine. Recording it makes a fix show up here as a diff.
			name: "claude-2026-07-numeric-session",
			stage: func(t *testing.T, w *world, username string) {
				stageClaudeSession(t, w, username, numericSessionID)
			},
		},
		{
			name: "cursor-2026-07",
			stage: func(t *testing.T, w *world, username string) {
				stageCursor(t, w, username, cursorConversation2026_07(), true)
			},
		},
		{
			// The drift case is a contract too: raw ships, the manifest says why.
			name: "cursor-2026-07-no-store",
			stage: func(t *testing.T, w *world, username string) {
				stageCursor(t, w, username, cursorConversation2026_07(), false)
			},
		},
		{
			// A store that deduplicated a re-run: the derived object ships, and the count of
			// what it could not enrich ships with it.
			name: "cursor-2026-07-repeat",
			stage: func(t *testing.T, w *world, username string) {
				stageCursor(t, w, username, cursorConversationRepeat(), true)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := stageWorld(t)
			username := realUsername(t)
			tc.stage(t, w, username)
			runOneShot(t)

			var got []goldenObject
			var payloads = map[string][]byte{}
			for _, o := range mirrorObjects(collect(t, w)) {
				if !isTranscript(o) {
					continue
				}
				g, payload := normalize(o, w, username)
				got = append(got, g)
				payloads[g.PayloadFile] = payload
			}
			if len(got) == 0 {
				t.Fatal("nothing was collected; a golden of nothing proves nothing")
			}
			compareGolden(t, tc.name, got, payloads)
		})
	}
}

// Strips everything the environment decides, keeps everything the shipper decides.
func normalize(o object, w *world, username string) (goldenObject, []byte) {
	payload := scrubEnvironment(o.Payload, w, username)

	g := goldenObject{
		Key:           o.Key,
		SourceID:      o.Manifest.SourceID,
		SourceFamily:  o.Manifest.SourceFamily,
		ArtifactClass: o.Manifest.ArtifactClass,
		Gather:        o.Manifest.Gather,
		NativePath:    string(scrubEnvironment([]byte(o.Manifest.NativePath), w, username)),
		ShapeSniff:    o.Manifest.ShapeSniff,

		ShippedHash:  o.Manifest.ShippedHash,
		PayloadSize:  o.Manifest.PayloadSize,
		PayloadLines: len(strings.Split(strings.TrimRight(string(o.Payload), "\n"), "\n")),

		Derived:          o.Manifest.Derived,
		EnrichStatus:     o.Manifest.EnrichStatus,
		EnrichMismatches: o.Manifest.EnrichMismatches,

		EnrichRepeats:          o.Manifest.EnrichRepeats,
		EnrichTail:             o.Manifest.EnrichTail,
		EnrichAmbiguous:        o.Manifest.EnrichAmbiguous,
		EnrichLineDecodeErrors: o.Manifest.EnrichLineDecodeErrors,
	}
	// The digest is over raw pre-redaction bytes that deliberately carry this machine's username,
	// so only its presence can be pinned; that it moves with the source is the invariants' business.
	if o.Manifest.SourceHash != "" {
		g.SourceHash = "<SOURCE-HASH>"
	}
	if o.Manifest.PayloadMTime != nil {
		g.PayloadMTime = o.Manifest.PayloadMTime.UTC().Format(time.RFC3339)
	}
	if o.Manifest.Enricher != nil {
		g.Enricher = fmt.Sprintf("%s@%d", o.Manifest.Enricher.ID, o.Manifest.Enricher.Version)
	}
	for _, from := range o.Manifest.DerivedFrom {
		g.DerivedFrom = append(g.DerivedFrom, string(scrubEnvironment([]byte(from), w, username)))
	}
	if r := o.Manifest.Redaction; r != nil && len(r.RuleHits) > 0 {
		g.Redaction = &goldenRedaction{Rules: r.RuleHits}
	}
	if e := o.Manifest.Encryption; e != nil {
		g.Recipients = e.RecipientKeyIDs
	}
	// One file per object, named for its source and key so a listing is readable and stable, with
	// the payload's real extension so a reader can open it with ordinary transcript tools.
	stem := g.SourceID + "-" + keyStem(o.Key)[:12]
	if g.Derived {
		stem = g.SourceID + "-derived-" + keyStem(o.Key)[:12]
	}
	g.PayloadFile = stem + ".jsonl"
	return g, payload
}

// The harness's temp directories and the account the tests run as; without this a golden records
// the runner.
func scrubEnvironment(b []byte, w *world, username string) []byte {
	s := string(b)
	for from, to := range map[string]string{
		w.Home:   "<HOME>",
		w.State:  "<STATE>",
		w.Config: "<CONFIG>",
	} {
		s = strings.ReplaceAll(s, from, to)
	}
	if len(username) >= 2 {
		s = strings.ReplaceAll(s, username, "<USER>")
	}
	return []byte(s)
}

func keyStem(key string) string {
	name := key[strings.LastIndex(key, "/")+1:]
	return strings.TrimSuffix(name, ".age")
}

// Payloads live beside the manifest record as their own files, so a redaction change reads as a
// text diff rather than a hash that moved.
func compareGolden(t *testing.T, name string, got []goldenObject, payloads map[string][]byte) {
	t.Helper()
	dir := filepath.Join("testdata", "golden", name)
	indexPath := filepath.Join(dir, "objects.json")

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	if *update {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "payloads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(indexPath, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		for file, body := range payloads {
			if err := os.WriteFile(filepath.Join(dir, "payloads", file), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("wrote %s", dir)
		return
	}

	want, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("no golden for %s: %v\nrun: go test ./e2e -update", name, err)
	}
	if string(want) != string(encoded) {
		t.Errorf("%s: the manifest record changed.\n%s", name, firstDifference(string(want), string(encoded)))
	}
	for file, body := range payloads {
		wantBody, err := os.ReadFile(filepath.Join(dir, "payloads", file))
		if err != nil {
			t.Errorf("no golden payload %s: %v", file, err)
			continue
		}
		if string(wantBody) != string(body) {
			t.Errorf("%s/%s: the shipped payload changed.\n%s", name, file,
				firstDifference(string(wantBody), string(body)))
		}
	}
}

// The first line that moved, with a hint: a diff a reader has to eyeball is a diff that gets
// skipped, and a skipped diff plus -update is how a regression becomes the expectation.
func firstDifference(want, got string) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var w, g string
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			return "line " + strconv.Itoa(i+1) + ":\n  want: " + strings.TrimSpace(w) +
				"\n  got:  " + strings.TrimSpace(g) +
				"\n\nIf the vendor changed shape, add a fixture generation beside this one." +
				"\nIf the shipper changed on purpose, read the whole diff, then -update."
		}
	}
	return "(no line differs; check trailing whitespace)"
}
