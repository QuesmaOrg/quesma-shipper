// The vend upload path end to end, hermetically: the tests whose subject is the path itself (the
// headers, the refusals, the expiries) rather than the collection behaviour it carries. What no
// unit test can show is the command tree, config layering, engine loop and uploader agreeing on
// one object's key, headers and bytes.
package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	seal "github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// --- the tests ----------------------------------------------------------------

// The whole path in one run: authorize, validate, PUT, commit. Every assertion is about what
// arrived at the store, the only thing a write-only client can get wrong unnoticed.
func TestVendPathShipsAuthorizedObjectsToTheStore(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))

	runOneShot(t)
	v.plane.assertClean(t)
	v.plane.assertOneWriter(t)

	mirrors := v.store.mirrorPuts()
	if len(mirrors) == 0 {
		t.Fatalf("nothing reached the object store; authorize batches: %v", v.plane.authorizeBatches())
	}

	for _, put := range mirrors {
		manifest, _, err := seal.Open(put.Body, v.Identity)
		if err != nil {
			t.Fatalf("%s: the stored bytes are not the sealed object: %v", put.Key, err)
		}
		if !strings.HasPrefix(put.Key, v.KeyRoot+"/mirror/") {
			t.Errorf("%s landed outside this install's mirror root %s", put.Key, v.KeyRoot)
		}
		// A header disagreeing with the manifest sealed inside would mean the two halves
		// describe different files.
		want := map[string]string{
			"x-amz-meta-source-hash":      manifest.SourceHash,
			"x-amz-meta-source-id":        manifest.SourceID,
			"x-amz-meta-manifest-version": fmt.Sprintf("%d", manifest.ManifestVersion),
			"x-amz-tagging":               "class=trajectory",
		}
		for name, value := range want {
			if got := put.Headers[name]; got != value {
				t.Errorf("%s: header %s is %q, want %q", put.Key, name, got, value)
			}
		}
		for _, name := range []string{"x-amz-meta-ticket-id", "x-amz-meta-shipped-hash", "x-amz-meta-artifact-class"} {
			if put.Headers[name] == "" {
				t.Errorf("%s: header %s did not arrive", put.Key, name)
			}
		}
	}

	// The heartbeat is an ordinary prepared upload under the one state key the protocol allows.
	beats := v.store.heartbeats()
	if len(beats) != 1 {
		t.Fatalf("the run wrote %d heartbeats, want exactly one", len(beats))
	}
	if got := beats[0].Headers["x-amz-meta-kind"]; got != "heartbeat" {
		t.Errorf("the heartbeat arrived with kind %q", got)
	}
	if got := beats[0].Headers["x-amz-tagging"]; got != "class=context" {
		t.Errorf("the heartbeat arrived tagged %q", got)
	}
	if _, _, err := seal.Open(beats[0].Body, v.Identity); err != nil {
		t.Errorf("the heartbeat is not a sealed object: %v", err)
	}
}

// Progress lives in the local fingerprint document and nowhere else: a second run with nothing
// changed must authorize no trajectory object, or every tick versions every file.
func TestVendPathShipsNothingOnASecondRun(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))
	// The generated project-map sidecar can legitimately change when nothing was collected, so it
	// is disabled here: the subject is files the client read off the machine.
	writeConfig(t, v, "sources:\n  - id: project-map\n    enabled: false\n")

	runOneShot(t)
	first := len(v.store.mirrorPuts())
	if first == 0 {
		t.Fatal("the first run shipped nothing, so the second proves nothing")
	}

	runOneShot(t)
	if got := len(v.store.mirrorPuts()); got != first {
		t.Errorf("the second run stored %d trajectory objects in total, want the first run's %d", got, first)
	}
	if shipped := shippedFromLog(t, v); len(shipped) != 0 {
		t.Errorf("the second run reported shipping %v", shipped)
	}
	// The heartbeat is current state, not history: rewritten every run whatever fingerprints say.
	if got := len(v.store.heartbeats()); got != 2 {
		t.Errorf("two runs wrote %d heartbeats, want one each", got)
	}
	v.plane.assertClean(t)
}

// A refused install stops the run and commits nothing, heartbeat included: writing one would be
// this install's last act reporting itself healthy.
func TestVendPathRefusalStopsTheRunAndTheHeartbeat(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))
	v.plane.setStatus(http.StatusForbidden)

	out, err := runOneShotExpectingFailure(t)
	if err == nil {
		t.Fatalf("a refused authorization exited zero:\n%s", out)
	}
	if !strings.Contains(out+err.Error(), "refused") {
		t.Errorf("the refusal was not reported as one:\n%s\n%v", out, err)
	}
	if got := v.store.stored(); len(got) != 0 {
		t.Fatalf("%d objects reached the store under a refusal", len(got))
	}

	// Nothing committed, so lifting the refusal ships everything on the next run.
	v.plane.setStatus(http.StatusOK)
	runOneShot(t)
	if len(v.store.mirrorPuts()) == 0 {
		t.Error("the run after the refusal was lifted shipped nothing")
	}
}

// Preview makes no network call: an authorization from it would tell the control plane about
// files this machine deliberately did not ship.
func TestVendPathPreviewAuthorizesNothing(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))

	out := run(t, "preview")
	if !strings.Contains(out, claudeSource) {
		t.Fatalf("preview decided nothing, so it proves nothing:\n%s", out)
	}
	if batches := v.plane.authorizeBatches(); len(batches) != 0 {
		t.Errorf("preview authorized %d batches: %v", len(batches), batches)
	}
	if got := v.store.stored(); len(got) != 0 {
		t.Errorf("preview uploaded %d objects", len(got))
	}
}

// A ticket that ran out between issue and PUT costs one fresh authorization and nothing else: an
// expiry is a slow upload, not a reason to leave the file for the next tick.
func TestVendPathReauthorizesOnceForAnExpiredTicket(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))
	v.plane.mu.Lock()
	v.plane.staleBatches = 1
	v.plane.mu.Unlock()

	runOneShot(t)
	v.plane.assertClean(t)

	mirrors := v.store.mirrorPuts()
	if len(mirrors) == 0 {
		t.Fatalf("the expired first batch was never retried; authorize batches: %v",
			v.plane.authorizeBatches())
	}
	// Authorized twice, stored once: the reauthorization replaced the dead ticket.
	authorized := map[string]int{}
	for _, batch := range v.plane.authorizeBatches() {
		for _, key := range batch {
			authorized[key]++
		}
	}
	retried := 0
	for _, n := range authorized {
		if n > 2 {
			t.Errorf("a key was authorized %d times; the client reauthorizes at most once", n)
		}
		if n == 2 {
			retried++
		}
	}
	if retried == 0 {
		t.Error("no key was authorized a second time, so no expiry was retried")
	}
	stored := map[string]int{}
	for _, put := range v.store.stored() {
		stored[put.Key]++
	}
	for key, n := range stored {
		if n != 1 {
			t.Errorf("%s was stored %d times; one prepared object is one PUT", key, n)
		}
	}
}

// doctor has nothing to read back here, so its write probe is a real write of the one state
// object the protocol authorizes.
func TestVendPathDoctorProbesByWritingTheHeartbeat(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))

	out := run(t, "doctor")
	// No conditional-write preflight exists on a write-only path.
	if !strings.Contains(out, "one test file sent") {
		t.Fatalf("doctor's upload probe did not report a successful heartbeat write:\n%s", out)
	}
	if strings.Contains(out, "preflight") {
		t.Errorf("doctor ran the conditional-write preflight against a write-only path:\n%s", out)
	}
	// Named by origin and never by a ticket URL, which anyone holding it could spend. Both
	// printing paths answer: doctor holds a live runtime, status the configuration alone.
	if !strings.Contains(out, v.store.server.URL) {
		t.Errorf("doctor did not name the upload destination:\n%s", out)
	}
	if status := run(t, "status"); !strings.Contains(status, v.store.server.URL) {
		t.Errorf("status did not name the upload destination:\n%s", status)
	}
	beats := v.store.heartbeats()
	if len(beats) != 1 {
		t.Fatalf("the probe wrote %d heartbeats, want exactly one", len(beats))
	}
	// A heartbeat naming no source would report a healthy install as one that found nothing.
	_, payload, err := seal.Open(beats[0].Body, v.Identity)
	if err != nil {
		t.Fatalf("the probe's heartbeat is not a sealed object: %v", err)
	}
	if !strings.Contains(string(payload), claudeSource) {
		t.Errorf("the probe's heartbeat carries no discovery health:\n%s", payload)
	}
	if len(v.store.mirrorPuts()) != 0 {
		t.Errorf("doctor shipped %d trajectory objects; it diagnoses, it does not collect",
			len(v.store.mirrorPuts()))
	}
}

// The whole point of persisting the tick outcome: a failed run uploads nothing, its own heartbeat
// included, so the failure has to survive on disk and ride a later run that can ship. Without this
// a machine that fails every tick is indistinguishable downstream from an idle one.
func TestAFailedRunsFailureRidesTheNextHeartbeat(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))

	// Every ticket names an origin no upload_targets entry admits, so the run ships nothing.
	elsewhere := startFakeStore(t)
	v.plane.store = elsewhere
	if _, err := runOneShotExpectingFailure(t); err == nil {
		t.Fatal("the staged sync was supposed to fail")
	}
	if len(v.store.heartbeats()) != 0 {
		t.Fatal("a run whose uploads all failed managed to ship a heartbeat")
	}

	// Recovered: the tickets are good again, and this run's heartbeat carries the earlier failure.
	v.plane.store = v.store
	runOneShot(t)

	beats := v.store.heartbeats()
	if len(beats) == 0 {
		t.Fatal("the recovered sync shipped no heartbeat")
	}
	_, payload, err := seal.Open(beats[len(beats)-1].Body, v.Identity)
	if err != nil {
		t.Fatalf("the heartbeat is not a sealed object: %v", err)
	}
	if !strings.Contains(string(payload), `"recent_failures"`) {
		t.Errorf("the heartbeat carries no record of the failed run:\n%s", payload)
	}
	if !strings.Contains(string(payload), `"kind": "tick_failed"`) {
		t.Errorf("the recorded failure is not classified:\n%s", payload)
	}
	if !strings.Contains(string(payload), "shipped nothing") {
		t.Errorf("the recorded failure does not say what went wrong:\n%s", payload)
	}
}

// doctor reads the local heartbeat mirror to answer "did anything leave this machine", and its
// own probe writes a heartbeat with every file counter zeroed. If the probe mirrored that, running
// the diagnostic would destroy the evidence and the next doctor would report a shipping install as
// one with nothing to ship.
func TestDoctorsProbeDoesNotOverwriteTheLastFlushMirror(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))

	runOneShot(t)
	mirror := filepath.Join(statePath(v), "heartbeat.json")
	shipped, err := os.ReadFile(mirror)
	if err != nil {
		t.Fatalf("a completed sync should have mirrored its heartbeat: %v", err)
	}
	if !strings.Contains(string(shipped), `"shipped"`) {
		t.Fatalf("the mirror records no shipped counter:\n%s", shipped)
	}

	run(t, "doctor")

	after, err := os.ReadFile(mirror)
	if err != nil {
		t.Fatalf("doctor removed the mirror: %v", err)
	}
	if string(after) != string(shipped) {
		t.Errorf("doctor's probe overwrote the last flush's mirror.\nbefore:\n%s\nafter:\n%s", shipped, after)
	}
}

// The one write path is compiled in, so no config names it, and the verbs an operator reaches for
// first must agree: disagreement is how a machine's real behaviour becomes unknowable.
func TestVendPathIsCompiledInAndReportedByTheReadOnlyVerbs(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))
	writeConfig(t, v, "")

	runOneShot(t)
	v.plane.assertClean(t)
	if len(v.store.mirrorPuts()) == 0 {
		t.Fatalf("nothing shipped over the compiled write path; batches: %v",
			v.plane.authorizeBatches())
	}
	// The read-only verbs must resolve the same document.
	if out := run(t, "status"); !strings.Contains(out, v.store.server.URL) {
		t.Errorf("status did not resolve the staged config:\n%s", out)
	}
	if out := run(t, "config"); !strings.Contains(out, "vend") {
		t.Errorf("config show did not report the compiled write path:\n%s", out)
	}
}

// The stranded-fleet regression: an enrolled install with no upload_targets must still run the
// flush rather than abort before the first network call. Nothing lands in this world because the
// store is plaintext http, which unpinned mode refuses per object.
func TestVendPathRunsUnpinnedWithNoUploadTargets(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))
	writeConfigWithoutUploadTargets(t, v)

	// Non-zero because it sent none of what it prepared, but the flush still RAN: the point of
	// this regression is that it reaches the network at all, which the summary below proves.
	out, err := runOneShotExpectingFailure(t)
	if err == nil {
		t.Error("a run that shipped none of what it prepared must not exit zero")
	}
	if len(v.plane.authorizeBatches()) == 0 {
		t.Fatalf("the flush aborted before authorizing anything:\n%s", out)
	}
	if got := v.store.stored(); len(got) != 0 {
		t.Errorf("%d objects reached a plaintext store no entry admitted", len(got))
	}
	if counts := summary(t, out); counts["failed"] == 0 || counts["shipped"] != 0 {
		t.Errorf("the run reported shipped %d failed %d; want every object refused per-object",
			counts["shipped"], counts["failed"])
	}
}

// The unpinned probe fails only on this world's plaintext store, and the refusal must name
// upload_targets as the way to admit one.
func TestVendPathDoctorReportsAnInstallWithNoUploadTargets(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))
	writeConfigWithoutUploadTargets(t, v)

	out, err := runExpectingFailure(t, "doctor")
	if err == nil {
		t.Errorf("doctor exited zero on an install whose probe cannot land:\n%s", out)
	}
	// The destination row carries the refusal, and the refusal names the setting.
	if !strings.Contains(out, "upload check failed") || !strings.Contains(out, "upload_targets") {
		t.Errorf("the destination row must name upload_targets:\n%s", out)
	}
	if got := v.store.stored(); len(got) != 0 {
		t.Errorf("%d objects reached the store over a refused scheme", len(got))
	}
	// Preview must still work: it computes everything that would leave and sends none of it.
	if preview := run(t, "preview"); !strings.Contains(preview, claudeSource) {
		t.Errorf("preview stopped working on an install with no upload path:\n%s", preview)
	}
}

// A ticket for the right origin and another install's key is refused before any byte is sent: the
// store would accept it, so the exact-key check is the whole defence against overwrites.
func TestVendPathRefusesATicketNamingAnotherInstallsKey(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))
	v.plane.mu.Lock()
	v.plane.misdirect = "00000000-0000-4000-8000-0000000000ff"
	v.plane.mu.Unlock()

	out, err := runOneShotExpectingFailure(t)
	if err == nil {
		t.Error("a run whose every ticket was refused must not exit zero")
	}
	if got := v.store.stored(); len(got) != 0 {
		t.Fatalf("%d objects were written under a key this install did not prepare: %s",
			len(got), got[0].Key)
	}
	if counts := summary(t, out); counts["failed"] == 0 || counts["shipped"] != 0 {
		t.Errorf("the run reported shipped %d failed %d; want every object failed and none shipped",
			counts["shipped"], counts["failed"])
	}
}

// Once the machine owner lists origins, a control plane naming another host must get nothing.
func TestVendPathRefusesATicketForAnUnlistedOrigin(t *testing.T) {
	v := stageWorld(t)
	stageClaude(t, v, realUsername(t))

	elsewhere := startFakeStore(t)
	v.plane.store = elsewhere

	// Non-zero: every upload failed, so the run shipped nothing. Exiting clean having sent none of
	// what it prepared is the outage shape this exit code exists to surface.
	out, err := runOneShotExpectingFailure(t)
	if err == nil {
		t.Error("a run whose every upload was refused must not exit zero")
	}
	if len(elsewhere.stored()) != 0 {
		t.Fatalf("%d objects were sent to an origin no upload_targets entry names", len(elsewhere.stored()))
	}
	if len(v.store.stored()) != 0 {
		t.Fatalf("%d objects reached the listed origin from tickets naming another", len(v.store.stored()))
	}
	// An ordinary per-object failure: nothing commits, the run reports it rather than stopping.
	if counts := summary(t, out); counts["failed"] == 0 || counts["shipped"] != 0 {
		t.Errorf("the run reported shipped %d failed %d; want every object failed and none shipped",
			counts["shipped"], counts["failed"])
	}
}
