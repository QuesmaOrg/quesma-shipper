package platform

// The offline pause switch: one file in the state directory. It carries its own end, so a
// forgotten pause never becomes months of silence; config.Resolve refuses state_dir from
// every non-local layer so nothing remote can move or clear it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const File = "paused"

type State struct {
	Paused bool   `json:"paused"`
	Reason string `json:"reason,omitempty"`
	At     string `json:"at"`
	Until  string `json:"until,omitempty"`
}

func (s State) UntilTime() time.Time {
	t, _ := time.Parse(time.RFC3339, s.Until)
	return t
}

func Set(stateDir, reason string, now, until time.Time) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	st := State{Paused: true, Reason: reason, At: now.UTC().Format(time.RFC3339)}
	if !until.IsZero() {
		st.Until = until.UTC().Format(time.RFC3339)
	}
	return WriteJSON(filepath.Join(stateDir, File), st, 0o644)
}

func Clear(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, File))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pause: %w", err)
	}
	return nil
}

// Read fails closed: a flag that exists but cannot be parsed reads as paused. A flag past its
// end reads as not paused; the file stays until the next Set or Clear.
func Read(stateDir string) State {
	return ReadAt(stateDir, time.Now())
}

func ReadAt(stateDir string, now time.Time) State {
	raw, _, err := ReadWhole(filepath.Join(stateDir, File), 64<<10)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}
		}
		return State{Paused: true, Reason: "the pause flag exists but could not be read: " + err.Error()}
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{Paused: true, Reason: "the pause flag exists but could not be parsed: " + err.Error()}
	}
	if until := s.UntilTime(); !until.IsZero() && !now.Before(until) {
		return State{}
	}
	s.Paused = true
	return s
}
