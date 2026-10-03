package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"
)

// Rows are written concurrently, and bounded the way the tags listing is: a full inventory against
// an object store is otherwise thousands of sequential round trips, past any load balancer's idle
// timeout, with the results table lost while the writes go on.
const metadataImportConcurrency = 8

var metadataKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var errInvalidMetadata = errors.New("invalid metadata")

func validateMetadataEntry(key, value string) error {
	if !metadataKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: keys must match ^[a-z][a-z0-9_]{0,31}$", errInvalidMetadata)
	}
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 256 {
		return fmt.Errorf("%w: values must contain at most 256 characters", errInvalidMetadata)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: values must not contain control characters", errInvalidMetadata)
		}
	}
	return nil
}

func validateMetadata(metadata map[string]string) error {
	if len(metadata) > 16 {
		return fmt.Errorf("%w: at most 16 keys are allowed", errInvalidMetadata)
	}
	for key, value := range metadata {
		if err := validateMetadataEntry(key, value); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) PatchMetadata(ctx context.Context, installID string, patch map[string]*string) error {
	if patch == nil {
		return fmt.Errorf("%w: metadata must be an object", errInvalidMetadata)
	}
	for key, value := range patch {
		text := ""
		if value != nil {
			text = *value
		}
		if err := validateMetadataEntry(key, text); err != nil {
			return err
		}
	}
	return m.updateTags(ctx, installID, func(rec *TagsRecord) error {
		if rec.Metadata == nil {
			rec.Metadata = make(map[string]string)
		}
		for key, value := range patch {
			if value == nil {
				delete(rec.Metadata, key)
			} else {
				rec.Metadata[key] = *value
			}
		}
		return validateMetadata(rec.Metadata)
	})
}

type metadataImportRow struct {
	Hostname  string             `json:"hostname"`
	Metadata  map[string]*string `json:"metadata"`
	InstallID string             `json:"install_id,omitempty"`
}

type metadataImportResult struct {
	Row        int      `json:"row"`
	Hostname   string   `json:"hostname"`
	Status     string   `json:"status"`
	InstallID  string   `json:"install_id,omitempty"`
	Candidates []string `json:"candidates,omitempty"`
	Message    string   `json:"message,omitempty"`
}

// hostnameMatchKey is what an inventory hostname and an enrolled one are compared by. The shipper
// enrolls with os.Hostname(), which on macOS is the Bonjour name ("Alices-MacBook.local") and on a
// managed network can be a full DNS name, while an MDM export carries the computer name in whatever
// case the device manager kept. Comparing the first label, case-folded, lets the two meet; the
// uniqueness rule still stands, so two machines that fold to one key are ambiguous, never guessed.
func hostnameMatchKey(hostname string) string {
	key := strings.ToLower(strings.TrimSpace(hostname))
	if label, _, found := strings.Cut(key, "."); found {
		key = label
	}
	return key
}

// Repeated hostnames in an inventory may represent different users; never let row order pick an owner.
func (m *Manager) ImportMetadata(ctx context.Context, rows []metadataImportRow, logger *log.Logger) ([]metadataImportResult, error) {
	if len(rows) == 0 || len(rows) > 1000 {
		return nil, fmt.Errorf("%w: import requires between 1 and 1000 rows", errInvalidMetadata)
	}
	hostRows := make(map[string]int)
	for i, row := range rows {
		if row.Hostname == "" || row.Metadata == nil {
			return nil, fmt.Errorf("%w: row %d requires hostname and metadata", errInvalidMetadata, i+1)
		}
		if err := validateMetadataEntry("hostname", row.Hostname); err != nil {
			return nil, fmt.Errorf("row %d hostname: %w", i+1, err)
		}
		if hostnameMatchKey(row.Hostname) == "" {
			return nil, fmt.Errorf("%w: row %d hostname is blank", errInvalidMetadata, i+1)
		}
		metadata := make(map[string]string, len(row.Metadata))
		for key, value := range row.Metadata {
			if value == nil {
				return nil, fmt.Errorf("%w: row %d values must be strings", errInvalidMetadata, i+1)
			}
			metadata[key] = *value
		}
		if err := validateMetadata(metadata); err != nil {
			return nil, fmt.Errorf("row %d: %w", i+1, err)
		}
		hostRows[hostnameMatchKey(row.Hostname)]++
	}
	installs, err := m.ListInstalls(ctx)
	if err != nil {
		return nil, err
	}
	byHost := make(map[string][]string)
	byID := make(map[string]bool)
	// Revocation preserves an identity; a shared hostname never proves its replacement owns the metadata.
	for _, install := range installs {
		if key := hostnameMatchKey(install.Hostname); key != "" {
			byHost[key] = append(byHost[key], install.InstallID)
		}
		byID[install.InstallID] = true
	}
	results := make([]metadataImportResult, len(rows))
	targetRows := make(map[string]int)
	for i, row := range rows {
		key := hostnameMatchKey(row.Hostname)
		result := metadataImportResult{Row: i + 1, Hostname: row.Hostname, Candidates: byHost[key]}
		switch {
		case row.InstallID != "":
			if byID[row.InstallID] {
				result.InstallID = row.InstallID
			} else {
				result.Status, result.Message = "unmatched", "Selected install does not belong to this organization"
			}
		case hostRows[key] > 1:
			result.Status, result.Message = "ambiguous", "Multiple inventory rows share this hostname; select each install manually"
		case len(result.Candidates) == 0:
			result.Status, result.Message = "unmatched", "No install has this hostname; select an install manually"
		case len(result.Candidates) > 1:
			result.Status, result.Message = "ambiguous", "Multiple install identities share this hostname; revoked installs also count. Select the intended install manually"
		default:
			result.InstallID = result.Candidates[0]
		}
		if result.InstallID != "" {
			targetRows[result.InstallID]++
		}
		results[i] = result
	}
	var group errgroup.Group
	group.SetLimit(metadataImportConcurrency)
	for i := range results {
		result := &results[i]
		if result.InstallID == "" {
			continue
		}
		if targetRows[result.InstallID] > 1 {
			result.Status, result.Message = "ambiguous", "Multiple inventory rows target this install; select distinct installs or import only the intended row"
			result.InstallID = ""
			continue
		}
		group.Go(func() error {
			err := m.PatchMetadata(ctx, result.InstallID, rows[i].Metadata)
			switch {
			case err == nil:
				result.Status = "imported"
			case errors.Is(err, errInvalidMetadata), errors.Is(err, ErrConflict):
				result.Status, result.Message = "error", err.Error()
			default:
				// The row says "retry"; the log says why, which the operator otherwise never learns.
				logger.Printf("metadata import row %d for install %s failed: %v", result.Row, result.InstallID, err)
				result.Status, result.Message = "error", "Metadata update unavailable; retry this row"
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return results, nil
}

func (s *Server) handleAdminPatchMetadata(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Metadata map[string]*string `json:"metadata"`
	}
	if !readAdminJSON(w, r, &req) {
		return
	}
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "update install metadata", manager.PatchMetadata(r.Context(), r.PathValue("id"), req.Metadata))
}

func (s *Server) handleAdminImportMetadata(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rows []metadataImportRow `json:"rows"`
	}
	if !readAdminJSON(w, r, &req) {
		return
	}
	manager, _ := s.adminManager(r)
	results, err := manager.ImportMetadata(r.Context(), req.Rows, s.logger)
	if err != nil {
		s.adminOperationError(w, "import install metadata", err)
		return
	}
	writeJSON(w, struct {
		Results []metadataImportResult `json:"results"`
	}{results})
}
