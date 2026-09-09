//go:build linux

// Rename-bridge glue: an agent that self-updated in place still runs as ~/.local/bin/shipper from a
// unit naming that path. Delete this file and its caller once no such install remains.
package linux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

const formerExecutable = "shipper"

// MigrateLegacyInstall renames this binary when it still runs under the former name and repoints
// the unit at the new one. The running process is left alone: it already is the new binary, and
// the unit only matters for the next start.
func MigrateLegacyInstall(out io.Writer) error {
	exe, err := common.CurrentExecutable()
	if err != nil {
		return err
	}
	if filepath.Base(exe) != formerExecutable {
		return nil
	}
	renamed := filepath.Join(filepath.Dir(exe), "quesma-shipper")
	if err := os.Rename(exe, renamed); err != nil {
		return err
	}
	fmt.Fprintf(out, "rename migration: %s is now %s\n", exe, renamed)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	changed, err := repointUnit(systemdPath(home), exe, renamed)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "rename migration: no unit here; point any crontab line at %s\n", renamed)
		return nil
	}
	if err != nil || !changed {
		return err
	}
	return daemonReload()
}

// repointUnit rewrites the unit's ExecStart from the former executable path to the renamed one.
func repointUnit(path, from, to string) (bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	old := "ExecStart=" + systemdQuote(from) + " "
	next := strings.Replace(string(raw), old, "ExecStart="+systemdQuote(to)+" ", 1)
	if next == string(raw) {
		return false, nil
	}
	return true, platform.WriteAtomic(path, []byte(next), common.EntryMode)
}
