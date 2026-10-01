package engine_test

// Copilot's canary shapes, modelled on real CLI 1.0.88 and Copilot Chat 0.57 stores. Credentials
// also go into the fields the copilot family exempts, which stand down only the entropy backstop.

import (
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

const copilotSession = "06dd1d48-5049-4272-948f-b3d83041ba32"

func copilotCLIRoot(home string) string {
	return filepath.Join(home, ".copilot", "session-state", copilotSession)
}

func vscodeUserDir(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Code", "User")
}

func vscodeWorkspaceDir(home string) string {
	return filepath.Join(vscodeUserDir(home), "workspaceStorage", "e7f61a270e86b41cc7bc4f2d7d4f3938")
}

func fileURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// copilotEvents is the CLI and VS Code transcript schema; context is empty for VS Code.
func copilotEvents(context string) string {
	var b strings.Builder
	b.WriteString(`{"type":"session.start","data":{"sessionId":"` + copilotSession + `","version":1,"producer":"copilot-agent","copilotVersion":"1.0.88","startTime":"2026-09-28T11:14:24.392Z"` + context + `},"id":"e0","timestamp":"2026-09-28T11:14:24.392Z","parentId":null}` + "\n")
	for i, l := range canaryLines() {
		q := jsonString(l)
		call := `"call_r4KeDgjw3zhljpnAV4CdlZT` + string(rune('a'+i)) + `"`
		b.WriteString(`{"type":"user.message","data":{"content":` + q + `,"transformedContent":` + q + `},"id":"u1","parentId":"e0"}` + "\n")
		b.WriteString(`{"type":"assistant.message","data":{"messageId":"m1","content":` + q + `,"toolRequests":[{"toolCallId":` + call + `,"name":"bash","arguments":{"command":` + q + `}}],"reasoningText":` + q + `,"encryptedContent":` + q + `,"reasoningBlocks":{"provider":"openai","blocks":[{"id":"rs_1","encrypted_content":` + q + `}]}},"id":"a1","parentId":"u1"}` + "\n")
		b.WriteString(`{"type":"tool.execution_start","data":{"toolCallId":` + call + `,"toolName":"bash","arguments":{"command":` + q + `}},"id":"t1","parentId":"a1"}` + "\n")
		b.WriteString(`{"type":"tool.execution_complete","data":{"toolCallId":` + call + `,"success":true,"result":{"content":` + q + `,"detailedContent":` + q + `}},"id":"t2","parentId":"t1"}` + "\n")
	}
	return b.String()
}

// vscodeChatSession is VS Code's operation log: a kind 0 snapshot, then kind 1 sets and kind 2
// splices at the path in k, each carrying a canary where the real store carries user text.
func vscodeChatSession() string {
	var b strings.Builder
	b.WriteString(`{"kind":0,"v":{"version":3,"creationDate":1790583502000,"sessionId":"` + copilotSession + `","requests":[]}}` + "\n")
	for i, l := range canaryLines() {
		q := jsonString(l)
		call := `call_r4KeDgjw3zhljpnAV4CdlZT` + string(rune('a'+i))
		b.WriteString(`{"kind":2,"k":["requests"],"v":[{"requestId":"request_1","message":{"text":` + q + `},"response":[{"kind":"markdownContent","value":` + q + `}],"variableData":{"variables":[{"id":"file","value":` + q + `}]}}]}` + "\n")
		b.WriteString(`{"kind":2,"k":["requests",0,"response"],"v":[{"kind":"toolInvocationSerialized","toolCallId":"` + call + `","invocationMessage":{"value":` + q + `}}]}` + "\n")
		b.WriteString(`{"kind":1,"k":["requests",0,"result"],"v":{"metadata":{"renderedUserMessage":[{"type":1,"text":` + q + `}],"toolCallRounds":[{"response":` + q + `,"toolCalls":[{"id":"` + call + `","name":"run_in_terminal","arguments":` + jsonString(`{"command":`+q+`}`) + `}],"thinking":{"id":"t1","encrypted":` + q + `},"statefulMarker":` + q + `}],"toolCallResults":{"` + call + `":{"$mid":20,"content":[{"$mid":21,"value":` + q + `}]}}}}}` + "\n")
	}
	return b.String()
}

// vscodeFlatSession is the flat <id>.json store: the kind 0 snapshot alone, on one line.
func vscodeFlatSession() string {
	var reqs []string
	for i, l := range canaryLines() {
		q := jsonString(l)
		call := `call_r4KeDgjw3zhljpnAV4CdlZT` + string(rune('a'+i))
		reqs = append(reqs, `{"requestId":"request_1","message":{"text":`+q+`},"response":[{"kind":"toolInvocationSerialized","toolCallId":"`+call+`","invocationMessage":{"value":`+q+`}}],"result":{"metadata":{"toolCallRounds":[{"toolCalls":[{"id":"`+call+`"}],"thinking":{"encrypted":`+q+`}}],"toolCallResults":{"`+call+`":{"content":[{"value":`+q+`}]}}}}}`)
	}
	return `{"version":3,"sessionId":"` + copilotSession + `","requests":[` + strings.Join(reqs, ",") + `]}`
}

func writeCopilotCLICanary(t *testing.T, home string) {
	t.Helper()
	context := `,"context":{"cwd":` + jsonString(filepath.Join(home, "work", "api")) + `,"repository":"acme/api","branch":"main"}`
	writeFile(t, filepath.Join(copilotCLIRoot(home), "events.jsonl"), []byte(copilotEvents(context)))
}

func writeCopilotCLIContextCanary(t *testing.T, home string) {
	t.Helper()
	dir := copilotCLIRoot(home)
	writeFile(t, filepath.Join(dir, "workspace.yaml"), []byte("id: "+copilotSession+"\ncwd: "+filepath.Join(home, "work", "api")+"\nname: rotate keys\n"+canaryText()))
	writeFile(t, filepath.Join(dir, "plan.md"), []byte("# Plan\n\n"+canaryText()))
	writeFile(t, filepath.Join(dir, "checkpoints", "001-rotate-keys.md"), []byte("# Checkpoint\n\n"+canaryText()))
	writeFile(t, filepath.Join(dir, "research", "keys.md"), []byte(canaryText()))
	writeFile(t, filepath.Join(dir, "files", "notes.txt"), []byte(canaryText()))
	writeFile(t, filepath.Join(dir, "rewind-file-snapshots", "3f9a0c1e"), []byte(canaryText()))
}

func writeVSCodeWorkspace(t *testing.T, home string) {
	t.Helper()
	writeFile(t, filepath.Join(vscodeWorkspaceDir(home), "workspace.json"), []byte(`{"folder":`+jsonString(fileURI(filepath.Join(home, "work", "api")))+`}`))
}

func init() {
	canaryFixtures["copilot-cli-sessions"] = writeCopilotCLICanary
	canaryFixtures["copilot-cli-context"] = writeCopilotCLIContextCanary
	canaryFixtures["copilot-vscode-transcripts"] = func(t *testing.T, home string) {
		writeVSCodeWorkspace(t, home)
		writeFile(t, filepath.Join(vscodeWorkspaceDir(home), "GitHub.copilot-chat", "transcripts", copilotSession+".jsonl"), []byte(copilotEvents("")))
	}
	canaryFixtures["copilot-cli-instructions"] = func(t *testing.T, home string) {
		root := filepath.Join(home, ".copilot")
		writeFile(t, filepath.Join(root, "copilot-instructions.md"), []byte("# Me\n\n"+canaryText()))
		writeFile(t, filepath.Join(root, "instructions", "go.instructions.md"), []byte(canaryText()))
	}
	canaryFixtures["copilot-vscode-chat-sessions"] = func(t *testing.T, home string) {
		writeVSCodeWorkspace(t, home)
		dir := filepath.Join(vscodeWorkspaceDir(home), "chatSessions")
		writeFile(t, filepath.Join(dir, copilotSession+".jsonl"), []byte(vscodeChatSession()))
		writeFile(t, filepath.Join(dir, "1ccc434c-2df3-481c-b8dc-7f1fd288c892.json"), []byte(vscodeFlatSession()))
	}
	canaryFixtures["copilot-vscode-global-sessions"] = func(t *testing.T, home string) {
		global := filepath.Join(vscodeUserDir(home), "globalStorage")
		writeFile(t, filepath.Join(global, "emptyWindowChatSessions", copilotSession+".jsonl"), []byte(vscodeChatSession()))
		writeFile(t, filepath.Join(global, "emptyWindowChatSessions", "aa824e47-b06b-4c2e-a9a4-b8b5cc1379d3.json"), []byte(vscodeFlatSession()))
		writeFile(t, filepath.Join(global, "transferredChatSessions", "3910e0b5-ce2d-4626-9001-7b7505958c58.json"), []byte(vscodeFlatSession()))
	}
	canaryFixtures["copilot-vscode-editing"] = func(t *testing.T, home string) {
		writeVSCodeWorkspace(t, home)
		dir := filepath.Join(vscodeWorkspaceDir(home), "chatEditingSessions", copilotSession)
		env := jsonString(canaryText())
		writeFile(t, filepath.Join(dir, "state.json"), []byte(`{"version":1,"initialFileContents":[[`+jsonString(fileURI(filepath.Join(home, "work", "api", ".env")))+`,`+env+`]],"timeline":{"checkpoints":[{"checkpointId":"c1","epoch":0,"label":"Initial"}]}}`))
		writeFile(t, filepath.Join(dir, "contents", "a1b2c3d4"), []byte(canaryText()))
	}
}
