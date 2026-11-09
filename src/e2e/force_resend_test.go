package e2e

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestForceResendAppliesCurrentRecipients(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(fmt.Sprintf("reset=%t", reset), func(t *testing.T) {
			w := stageWorld(t)
			stageClaudeSession(t, w, realUsername(t), claudeSessionID)
			alice, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			bob, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			configure := func(id *age.X25519Identity) {
				writeConfig(t, w, "sources:\n  - id: claude-account\n    enabled: false\nencryption:\n  include_install_recipient: false\n  additional_recipients:\n    - "+id.Recipient().String()+"\n")
			}
			configure(alice)
			runOneShot(t)
			old := w.store.mirrorPuts()
			if len(old) != 1 {
				t.Fatalf("initial uploads: %d", len(old))
			}
			_, before, err := transforms.Open(old[0].Body, alice)
			if err != nil {
				t.Fatal(err)
			}
			configure(bob)
			if reset {
				run(t, "state", "reset", "--apply")
			}
			runOneShot(t)
			if got := len(w.store.mirrorPuts()); got != 1 {
				t.Fatalf("normal run wrote %d copies", got)
			}
			runOneShot(t, "--force-resend")
			puts := w.store.mirrorPuts()
			if len(puts) != 2 || puts[0].Key != puts[1].Key {
				t.Fatalf("force resend did not version the original key: %+v", keysOf(puts))
			}
			_, after, err := transforms.Open(puts[1].Body, bob)
			if err != nil {
				t.Fatalf("new reader cannot open replacement: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("recipient change changed the archived text")
			}
			if _, _, err := transforms.Open(puts[1].Body, alice); err == nil {
				t.Fatal("replacement retained the old reader")
			}
			for _, name := range []string{"x-amz-meta-source-hash", "x-amz-meta-shipped-hash"} {
				if puts[0].Headers[name] != puts[1].Headers[name] {
					t.Fatalf("force resend changed %s", name)
				}
			}
			runOneShot(t)
			run(t, "state", "reset", "--apply")
			runOneShot(t)
			if got := len(w.store.mirrorPuts()); got != 2 {
				t.Fatalf("normal dedup stopped working: %d versions", got)
			}
			w.plane.assertClean(t)
		})
	}
}

func keysOf(puts []storedPut) []string {
	keys := make([]string, len(puts))
	for i, put := range puts {
		keys[i] = put.Key
	}
	return keys
}

func TestForceResendScrubsAgainAndDrainsPastTheNormalLimit(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	appendLine(t, stageClaudeSession(t, w, username, claudeSessionID), houseLine)
	stageClaudeSession(t, w, username, numericSessionID)
	const base = "sources:\n  - id: claude-account\n    enabled: false\n"
	writeConfig(t, w, base)
	runOneShot(t)
	if len(w.store.mirrorPuts()) != 2 {
		t.Fatal("expected two original uploads")
	}
	writeConfig(t, w, base+"scrub:\n  secret_key_names:\n    - HOUSE_SIG\n")
	configPath := filepath.Join(w.Config, "trajectory-shipper", "config.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, bytes.ReplaceAll(raw, []byte("max_files_per_run: 10000"), []byte("max_files_per_run: 1")), 0o600); err != nil {
		t.Fatal(err)
	}
	runOneShot(t, "--force-resend")
	puts := w.store.mirrorPuts()
	if len(puts) != 4 {
		t.Fatalf("force resend did not drain both files: %d uploads", len(puts))
	}
	for _, put := range puts[2:] {
		_, payload, err := transforms.Open(put.Body, w.Identity)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), houseSecret) {
			t.Fatal("replacement kept text the new rule hides")
		}
		if w.store.versions(put.Key) != 2 {
			t.Fatal("force resend changed the object key")
		}
	}
	runOneShot(t)
	if len(w.store.mirrorPuts()) != 4 {
		t.Fatal("force resend leaked into the next run")
	}
	w.plane.assertClean(t)
}

func TestForceResendFailurePreservesStateAndCanBeRetried(t *testing.T) {
	w := stageWorld(t)
	stageClaudeSession(t, w, realUsername(t), claudeSessionID)
	writeConfig(t, w, "sources:\n  - id: claude-account\n    enabled: false\n")
	runOneShot(t)
	state := filepath.Join(statePath(w), engine.FileName)
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	w.plane.setStatus(http.StatusServiceUnavailable)
	if _, err := runOneShotExpectingFailure(t, "--force-resend"); err == nil {
		t.Fatal("failed force resend returned success")
	}
	after, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed upload changed the successful fingerprint")
	}
	if len(w.store.mirrorPuts()) != 1 {
		t.Fatal("failed force resend wrote a copy")
	}
	w.plane.setStatus(http.StatusOK)
	runOneShot(t, "--force-resend")
	if len(w.store.mirrorPuts()) != 2 {
		t.Fatal("force resend did not retry")
	}
}

func TestForceResendRequiresOnceAndHonorsPause(t *testing.T) {
	if _, err := runExpectingFailure(t, "run", "--force-resend"); err == nil || !strings.Contains(err.Error(), "requires --once") {
		t.Fatalf("scheduler accepted force resend: %v", err)
	}
	w := stageWorld(t)
	stageClaudeSession(t, w, realUsername(t), claudeSessionID)
	run(t, "pause", "1h")
	if _, err := runOneShotExpectingFailure(t, "--force-resend"); err == nil {
		t.Fatal("paused force resend returned success")
	}
	if len(w.store.mirrorPuts()) != 0 {
		t.Fatal("force resend ignored pause")
	}
}

func TestForceResendReportsPartialFailure(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageClaudeSession(t, w, username, claudeSessionID)
	stageClaudeSession(t, w, username, numericSessionID)
	writeConfig(t, w, "sources:\n  - id: claude-account\n    enabled: false\n")
	runOneShot(t)
	puts := w.store.mirrorPuts()
	if len(puts) != 2 {
		t.Fatal("expected two initial copies")
	}
	w.store.mu.Lock()
	w.store.putStatus = map[string]int{puts[0].Key: http.StatusInternalServerError}
	w.store.mu.Unlock()
	_, err := runOneShotExpectingFailure(t, "--force-resend")
	if err == nil || !strings.Contains(err.Error(), "force resend incomplete") {
		t.Fatalf("partial failure returned %v", err)
	}
	if w.store.versions(puts[0].Key) != 1 || w.store.versions(puts[1].Key) != 2 {
		t.Fatal("partial upload did not preserve independent results")
	}
}

func TestForceResendCancellationIsNotSuccess(t *testing.T) {
	w := stageWorld(t)
	stageClaudeSession(t, w, realUsername(t), claudeSessionID)
	if _, err := runUntilCancelled(t, "run", "--once", "--force-resend"); err == nil {
		t.Fatal("cancelled force resend returned success")
	}
	if len(w.store.mirrorPuts()) != 0 {
		t.Fatal("cancelled force resend uploaded a file")
	}
}
