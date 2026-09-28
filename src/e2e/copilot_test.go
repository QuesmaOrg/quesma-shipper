package e2e

// Copilot through the whole client: a CLI session and a VS Code workspace staged where each OS
// keeps them, one sync, and the objects the store received. The same invariants the Claude Code
// fixture holds: no seeded secret and no OS username survives.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const copilotSessionID = "06dd1d48-5049-4272-948f-b3d83041ba32"

// vscodeUser is where VS Code keeps user data on this OS, under the staged environment.
func vscodeUser(w *world) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(w.Home, "Library", "Application Support", "Code", "User")
	case "windows":
		return filepath.Join(w.Home, "AppData", "Roaming", "Code", "User")
	}
	return filepath.Join(w.Config, "Code", "User")
}

func stageAbs(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, path)
}

// One CLI session and one Copilot Chat session carrying the seeded secrets in tool output, and the
// shapeless one under a field that names it (as the Claude Code fixture does: free text such as
// "api_key: value" is not caught for any agent). The OS username is in every path.
func stageCopilot(t *testing.T, w *world, username string) {
	t.Helper()
	cwd := "/Users/" + username + "/work/demo"
	call := "call_r4KeDgjw3zhljpnAV4CdlZT0"
	events := strings.Join([]string{
		fmt.Sprintf(`{"type":"session.start","data":{"sessionId":%q,"version":1,"producer":"copilot-agent","copilotVersion":"1.0.88","context":{"cwd":%q}},"id":"e0","parentId":null}`, copilotSessionID, cwd),
		`{"type":"user.message","data":{"content":"deploy the api"},"id":"u1","parentId":"e0"}`,
		fmt.Sprintf(`{"type":"tool.execution_start","data":{"toolCallId":%q,"toolName":"db_connect","arguments":{"path":"%s/.env","password":%q}},"id":"t1","parentId":"u1"}`, call, cwd, seededShapelessSecret),
		fmt.Sprintf(`{"type":"tool.execution_complete","data":{"toolCallId":%q,"success":true,"result":{"content":"GITHUB_TOKEN=%s\nAWS_ACCESS_KEY_ID=%s"}},"id":"t2","parentId":"t1"}`, call, seededGitHubToken, seededAWSKey),
	}, "\n") + "\n"
	session := filepath.ToSlash(filepath.Join(".copilot", "session-state", copilotSessionID))
	stageFile(t, w, session+"/events.jsonl", events)
	stageFile(t, w, session+"/plan.md", "# Plan\n\nRotate "+seededGitHubToken+" in "+cwd+"\n")

	ws := filepath.Join(vscodeUser(w), "workspaceStorage", "e7f61a270e86b41cc7bc4f2d7d4f3938")
	stageAbs(t, filepath.Join(ws, "workspace.json"), `{"folder":"file://`+cwd+`"}`)
	stageAbs(t, filepath.Join(ws, "chatSessions", copilotSessionID+".jsonl"), strings.Join([]string{
		fmt.Sprintf(`{"kind":0,"v":{"version":3,"sessionId":%q,"requests":[]}}`, copilotSessionID),
		fmt.Sprintf(`{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"toolCallRounds":[{"toolCalls":[{"id":%q,"arguments":{"api_key":%q}}]}],"toolCallResults":{%q:{"content":[{"value":"GITHUB_TOKEN=%s AWS_ACCESS_KEY_ID=%s"}]}}}}}`, call, seededShapelessSecret, call, seededGitHubToken, seededAWSKey),
		fmt.Sprintf(`{"kind":2,"k":["requests",0,"response"],"v":[{"invocationMessage":{"value":"Read [](file://%s/.env)"}}]}`, cwd),
	}, "\n")+"\n")
}

// copilotObjects are this sync's objects from the copilot family, with every staged source present.
func copilotObjects(t *testing.T, w *world) []object {
	t.Helper()
	var out []object
	sources := map[string]bool{}
	for _, o := range collect(t, w) {
		if strings.HasPrefix(o.Manifest.SourceID, "copilot-") {
			out = append(out, o)
			sources[o.Manifest.SourceID] = true
		}
	}
	for _, id := range []string{"copilot-cli-sessions", "copilot-cli-context", "copilot-vscode-chat-sessions"} {
		if !sources[id] {
			t.Fatalf("%s shipped nothing; the rest of this test would pass vacuously", id)
		}
	}
	return out
}

func TestNoCopilotByteCarriesASeededSecret(t *testing.T) {
	w := stageWorld(t)
	stageCopilot(t, w, realUsername(t))
	runOneShot(t)

	for _, o := range copilotObjects(t, w) {
		for _, secret := range []string{seededGitHubToken, seededAWSKey, seededShapelessSecret} {
			if strings.Contains(string(o.Payload), secret) {
				t.Errorf("%s (%s): a seeded secret survived redaction: %s", o.Key, o.Manifest.SourceID, secret)
			}
		}
	}
}

func TestNoCopilotByteCarriesTheOSUsername(t *testing.T) {
	w := stageWorld(t)
	username := realUsername(t)
	stageCopilot(t, w, username)
	runOneShot(t)

	for _, o := range copilotObjects(t, w) {
		if strings.Contains(string(o.Payload), username) {
			t.Errorf("%s (%s): the OS username survived in the payload", o.Key, o.Manifest.SourceID)
		}
		if strings.Contains(o.Manifest.NativePath, username) {
			t.Errorf("%s (%s): the OS username survived in native_path: %s", o.Key, o.Manifest.SourceID, o.Manifest.NativePath)
		}
	}
}
