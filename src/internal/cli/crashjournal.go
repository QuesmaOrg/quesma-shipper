// The collecting verbs' entry into the crash journal: report how the previous run died, then
// mark this one's start. Best-effort throughout — a broken journal must never stop shipping.
package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/crashjournal"
)

func startCrashJournal(errOut io.Writer) (*crashjournal.Log, string, *formats.LastCrash) {
	runID := newRunID()
	dir, err := app.StateDirWithoutConfig()
	if err != nil {
		return nil, runID, nil
	}

	// Before Open, which may rotate the file this reads.
	prev := crashjournal.LastRun(dir)

	fl, err := crashjournal.Open(dir, runID)
	if err != nil {
		fmt.Fprintf(errOut, "warning: crash journal unavailable: %v\n", err)
		return nil, runID, nil
	}
	fl.Start()

	var crash *formats.LastCrash
	if prev != nil && !prev.Clean {
		crash = &formats.LastCrash{RunID: prev.RunID, Phase: prev.Phase, Consecutive: prev.Crashes}
		fmt.Fprintf(errOut, "previous run %s never exited: last step %q; %d consecutive unclean run(s)\n",
			prev.RunID, prev.Phase, prev.Crashes)
	}
	return fl, runID, crash
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
