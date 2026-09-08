package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/app"
)

// The verb is what a reader needs to tell a crash in `enroll` from one in the daemon.
func TestVerbOfNamesTheFirstNonFlagArgument(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"quesma-shipper", "sync"}, "sync"},
		{[]string{"quesma-shipper", "-q", "sync"}, "sync"},
		{[]string{"quesma-shipper", "state", "prune"}, "state"},
		{[]string{"quesma-shipper", "--version"}, "quesma-shipper"},
		{[]string{"quesma-shipper"}, "quesma-shipper"},
	} {
		if got := verbOf(tc.args); got != tc.want {
			t.Errorf("verbOf(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// A crash in any verb must outlive the terminal it printed to: the stack goes to stderr, the fact
// of it goes to disk, where the next heartbeat that ships will find it.
func TestReportPanicPrintsTheStackAndPersistsTheFact(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var errOut bytes.Buffer
	reportPanic(&errOut, []string{"quesma-shipper", "enroll", "--invite", "x"}, "boom")

	if !strings.Contains(errOut.String(), "panic: boom") {
		t.Errorf("the panic did not reach stderr:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "runtime/debug.Stack") &&
		!strings.Contains(errOut.String(), "goroutine") {
		t.Errorf("the stack did not reach stderr:\n%s", errOut.String())
	}

	dir, err := app.StateDirWithoutConfig()
	if err != nil {
		t.Skipf("no resolvable state directory here: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "last-failure.json"))
	if err != nil {
		t.Fatalf("the crash was not persisted under %s: %v", dir, err)
	}
	// The verb, so a crash in enroll is distinguishable from one in the daemon.
	if !strings.Contains(string(raw), "enroll") || !strings.Contains(string(raw), "boom") {
		t.Errorf("the record does not say what crashed:\n%s", raw)
	}
	// The stack must NOT be there: it is the one diagnostic that can carry payload text.
	if strings.Contains(string(raw), "goroutine") {
		t.Errorf("a stack reached the persisted record:\n%s", raw)
	}
}
