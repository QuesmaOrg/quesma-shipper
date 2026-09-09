//go:build linux

// Rename-bridge glue: an agent that self-updated in place still runs as ~/.local/bin/shipper from a
// unit naming that path. Delete this file and its caller once no such install remains.
package linux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/packaging/common"
)

// MigrateLegacyInstall renames this binary when it still runs under the former name and repoints
// the unit at the new one. The running process is left alone: it already is the new binary, and
// the unit only matters for the next start.
func MigrateLegacyInstall(out io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	renamed, ok := common.RenamedExecutable(exe)
	if !ok {
		return nil
	}
	if err := os.Rename(exe, renamed); err != nil {
		return err
	}
	fmt.Fprintf(out, "rename migration: %s is now %s\n", exe, renamed)
	if !Available() {
		fmt.Fprintf(out, "rename migration: no systemd --user here; point your crontab line at %s\n", renamed)
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	changed, err := repointUnit(systemdPath(home), exe, renamed)
	if err != nil || !changed {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// repointUnit rewrites the unit's ExecStart from the former executable path to the renamed one.
func repointUnit(path, from, to string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(raw), "\n")
	changed := false
	for i, line := range lines {
		if strings.HasPrefix(line, "ExecStart=") && strings.Contains(line, systemdQuote(from)) {
			lines[i] = strings.Replace(line, systemdQuote(from), systemdQuote(to), 1)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, platform.WriteAtomic(path, []byte(strings.Join(lines, "\n")), common.EntryMode)
}
