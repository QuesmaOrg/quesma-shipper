//go:build perf

// A synthetic machine and the built binaries, staged for measurement: the protocol tier's staging
// harness copied rather than shared, with the assertions dropped and the timing added. Every world
// is a fresh install, so each owns a subtree and no world counts another's objects.
package perf

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Captured before anything overrides HOME: a `go build` under a synthetic HOME re-downloads the
// module cache into a temp directory the test framework then cannot delete.
var realHome = os.Getenv("HOME")

// Fixed, so object keys (HMACs under name_key) match across worlds and two runs stay comparable key
// by key. Published in a public repository: it protects nothing and must never be used elsewhere.
const (
	testAgeIdentity = "AGE-SECRET-KEY-1JF0Y36Z2RMJNJNN2AYUUF6HMHZVK3FCGK4GUADGRF9M3R57S2UCSDMJWD7"
	testNameKey     = "0101010101010101010101010101010101010101010101010101010101010101"
)

// The pre-filter compares size and mtime, so staged files cannot carry a wall-clock stamp.
var fixtureMTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// Pins the child's view of the machine: the engine sizes its pools off GOMAXPROCS, so unpinned, every
// bound would have to be written for the weakest box. A scenario that changes it writes its own bounds.
const childGOMAXPROCS = 8

// Generous on purpose: a deadlock catcher, not a performance bound.
const syncTimeout = 10 * time.Minute

// One synthetic machine enrolled against the protocol peer and allowed exactly one upload origin.
// The settings are fields rather than stageWorld arguments: a scenario sets at most one of them.
type world struct {
	Home   string
	Config string // XDG_CONFIG_HOME
	State  string // XDG_STATE_HOME

	// installID changes with every staging and reset; keyRoot is the subtree it gives this world.
	installID string
	org       string
	keyRoot   string

	// The store the tickets this world accepts may name, and the prefix under it.
	origin string
	bucket string

	binary string

	// gomaxprocs is the child's view of the machine. See childGOMAXPROCS.
	gomaxprocs int

	// Appended last, so a scenario can hand the child a limit of its own without a launcher.
	extraEnv []string

	// Goes in front of the binary when the child must run inside a cap; a prefix, not a replacement,
	// so the launch path stays the same. A launcher that resets the environment carries childVars itself.
	launcher []string
}

// Environment rather than flags, because that is how a real install is configured.
func stageWorld(t *testing.T) *world {
	t.Helper()
	return stageWorldIn(t, perfOrg, storeEndpoint, perfBucket)
}

func stageWorldIn(t *testing.T, org, origin, bucket string) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{
		Home:       filepath.Join(root, "home"),
		Config:     filepath.Join(root, "config"),
		State:      filepath.Join(root, "state"),
		org:        org,
		origin:     origin,
		bucket:     bucket,
		binary:     shipperBinary(t),
		gomaxprocs: childGOMAXPROCS,
	}
	for _, dir := range []string{w.Home, w.Config, w.State} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	w.reset(t)
	return w
}

// Everything a run remembers lives in the state directory, so emptying it and enrolling again is a
// first sync. A new install id, not the old one: the fingerprint document is refused under another.
func (w *world) reset(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(w.State, "trajectory-shipper")); err != nil {
		t.Fatalf("clear the state directory: %v", err)
	}
	w.installID = uuid.New().String()
	w.keyRoot = "v1/organization=" + w.org + "/install=" + w.installID
	seedIdentity(t, w)
	writeClientConfig(t, w)
	seedEnrollment(t, w)
	allowUploadTarget(t, w)
}

func seedIdentity(t *testing.T, w *world) {
	t.Helper()
	id, err := age.ParseX25519Identity(testAgeIdentity)
	if err != nil {
		t.Fatalf("the test identity does not parse: %v", err)
	}
	dir := filepath.Join(w.State, "trajectory-shipper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := map[string]any{
		"identity_schema": 1,
		"install_id":      w.installID,
		"age_identity":    testAgeIdentity,
		"age_recipient":   id.Recipient().String(),
		"name_key":        testNameKey,
		"created_at":      fixtureMTime.Format(time.RFC3339),
	}
	raw, err := json.Marshal(unit)
	if err != nil {
		t.Fatal(err)
	}
	// 0600: Load refuses a unit any wider.
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Carries no send block on purpose: the destination arrives in the signed document.
func writeClientConfig(t *testing.T, w *world) {
	t.Helper()
	dir := filepath.Join(w.Config, "trajectory-shipper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "config_version: 1\n" +
		// The 64-file default would truncate the corpus; this must stay above corpusFiles.
		"max_files_per_run: 100000\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Enrollment is fixture state here: performance measures the shipper, not a particular control plane.
func seedEnrollment(t *testing.T, w *world) {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	peer.register(w.installID, w.org, pub, w.origin, w.bucket)
	record := map[string]any{
		"enrollment_schema": 2,
		"install_id":        w.installID,
		"organization":      w.org,
		"endpoint":          peer.url,
		"device_key":        base64.StdEncoding.EncodeToString(private),
		"enrolled_at":       fixtureMTime.Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.State, "trajectory-shipper", "enrollment.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The machine owner's half of the presigned path: this list alone decides whether a ticket's origin
// may be spoken to, and no served layer can add to it.
func allowUploadTarget(t *testing.T, w *world) {
	t.Helper()
	path := filepath.Join(w.Config, "trajectory-shipper", "config.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no user config to pin an upload target in: %v", err)
	}
	// The proxy serves plain HTTP, and the client refuses a cleartext origin nobody opted into.
	block := "upload_targets:\n" +
		"  - origin: " + w.origin + "\n" +
		"    addressing: path-style\n" +
		"    path_prefix: /" + w.bucket + "\n" +
		"    allow_loopback_http: true\n"
	if err := os.WriteFile(path, append(body, block...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --- the binaries, built once ------------------------------------------------

var (
	buildOnce   sync.Once
	buildDir    string
	shipperPath string
	buildErr    error
)

func buildBinaries() {
	dir, err := os.MkdirTemp("", "shipper-perf")
	if err != nil {
		buildErr = err
		return
	}
	buildDir = dir
	build := func(out, moduleDir, pkg string) error {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = moduleDir
		// The REAL home: see realHome.
		cmd.Env = append(os.Environ(), "HOME="+realHome)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %v: %s", pkg, err, b)
		}
		return nil
	}
	shipperPath = filepath.Join(dir, "quesma-shipper")
	buildErr = build(shipperPath, "..", "./cmd/shipper")
}

// The client under measurement, built once for the whole tier.
func shipperBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(buildBinaries)
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return shipperPath
}

// Package level, so there is no t.TempDir to do it: every run would otherwise leak two binaries.
func removeBinaries() {
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
}

// --- the child's world -------------------------------------------------------

// Everything this world tells the child about itself, and nothing inherited; separate from childEnv
// because a launcher such as sudo resets what it was called with. It names no store and no credential.
func (w *world) childVars() []string {
	vars := []string{
		"HOME=" + w.Home,
		"XDG_CONFIG_HOME=" + w.Config,
		"XDG_STATE_HOME=" + w.State,
		fmt.Sprintf("GOMAXPROCS=%d", w.gomaxprocs),
	}
	// Last, so a scenario's own limit wins over anything above it.
	return append(vars, w.extraEnv...)
}

// This process's environment minus anything that would move the measurement, then this world's own
// variables on top: an exported GOMEMLIMIT or SHIPPER_* would retune the run with nothing to show it.
func (w *world) childEnv() []string {
	inherited := os.Environ()
	env := make([]string, 0, len(inherited)+len(w.childVars()))
	for _, kv := range inherited {
		if measuredVar(kv) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, w.childVars()...)
}

// Variables that must reach the child from this world or not at all; childVars names the wanted ones.
func measuredVar(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	switch name {
	case "HOME", "GOMAXPROCS", "GOMEMLIMIT", "GOGC":
		return true
	}
	return strings.HasPrefix(name, "SHIPPER_") ||
		strings.HasPrefix(name, "XDG_") ||
		strings.HasPrefix(name, "AWS_")
}

// A scenario that needs to survive a failed run uses observedSync directly.
func (w *world) mustSync(t *testing.T) childObservation {
	t.Helper()
	obs := w.observedSync(t)
	if obs.Err != nil {
		t.Fatalf("shipper run --once: %v\n%s", obs.Err, obs.Output)
	}
	return obs
}

// One sync as a user would run it: what it printed and how long it took.
func (w *world) runSync(t *testing.T) (string, time.Duration) {
	t.Helper()
	obs := w.mustSync(t)
	return obs.Output, obs.Elapsed
}

// The run's own counters, read from its summary row; counting objects in the store cannot answer
// "did this run ship anything", because an overwritten key leaves the listing unchanged. Only a
// single line carrying all five words counts: any other line with a number would overwrite the verdict.
func summary(t *testing.T, out string) map[string]int {
	t.Helper()
	for line := range strings.SplitSeq(out, "\n") {
		if counts, ok := summaryLine(line); ok {
			return counts
		}
	}
	t.Fatalf("no run summary in the output:\n%s", out)
	return nil
}

// The whole row, in the order the client prints it.
var summaryWords = []string{"shipped", "unchanged", "skipped", "parked", "failed"}

// Accepted only when every word carried a whole number; Atoi rather than Sscanf: "12abc" is not twelve.
func summaryLine(line string) (map[string]int, bool) {
	counts := map[string]int{}
	fields := strings.Fields(line)
	for i := 0; i+1 < len(fields); i++ {
		if !slices.Contains(summaryWords, fields[i]) {
			continue
		}
		n, err := strconv.Atoi(fields[i+1])
		if err != nil {
			return nil, false
		}
		counts[fields[i]] = n
	}
	return counts, len(counts) == len(summaryWords)
}

// --- what landed in the store ------------------------------------------------

// Relative to the install root, which changes with every reset; the key under it is what two runs share.
func (w *world) currentKeys(t *testing.T) []string {
	t.Helper()
	var keys []string
	p := awss3.NewListObjectsV2Paginator(adminS3, &awss3.ListObjectsV2Input{
		Bucket: aws.String(w.bucket),
		Prefix: aws.String(w.keyRoot + "/"),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		if err != nil {
			t.Fatalf("list keys: %v", err)
		}
		for _, o := range page.Contents {
			keys = append(keys, strings.TrimPrefix(aws.ToString(o.Key), w.keyRoot+"/"))
		}
	}
	return keys
}

// Versions per key, keyed relative as above; ListObjectVersions has no generated paginator.
func (w *world) versionCounts(t *testing.T) map[string]int {
	t.Helper()
	counts := map[string]int{}
	in := &awss3.ListObjectVersionsInput{
		Bucket: aws.String(w.bucket),
		Prefix: aws.String(w.keyRoot + "/"),
	}
	for {
		out, err := adminS3.ListObjectVersions(context.Background(), in)
		if err != nil {
			t.Fatalf("list versions: %v", err)
		}
		for _, v := range out.Versions {
			counts[strings.TrimPrefix(aws.ToString(v.Key), w.keyRoot+"/")]++
		}
		if !aws.ToBool(out.IsTruncated) {
			return counts
		}
		in.KeyMarker, in.VersionIdMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
}
