package common

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

const SelfUpdateHopName = "selfupdate_hop"

func ReadSelfUpdateHop(stateDir string) string {
	raw, _, err := platform.ReadWhole(filepath.Join(stateDir, SelfUpdateHopName), 256)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func WriteSelfUpdateHop(stateDir, version string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	return platform.WriteAtomic(filepath.Join(stateDir, SelfUpdateHopName), []byte(version+"\n"), 0o600)
}

func ClearSelfUpdateHop(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, SelfUpdateHopName))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
