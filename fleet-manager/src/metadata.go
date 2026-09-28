package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"unicode"
	"unicode/utf8"
)

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

// Repeated hostnames in an inventory may represent different users; never let row order pick an owner.
func (m *Manager) ImportMetadata(ctx context.Context, rows []metadataImportRow) ([]metadataImportResult, error) {
	if len(rows) == 0 || len(rows) > 1000 {
		return nil, fmt.Errorf("%w: import requires between 1 and 1000 rows", errInvalidMetadata)
	}
	hostRows := make(map[string]int)
	for i, row := range rows {
		if row.Hostname == "" || row.Metadata == nil {
			return nil, fmt.Errorf("%w: row %d requires hostname and metadata", errInvalidMetadata, i+1)
		}
		if err := validateMetadataEntry("hostname", row.Hostname); err != nil {
			return nil, fmt.Errorf("row %d: %w", i+1, err)
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
		hostRows[row.Hostname]++
	}
	installs, err := m.ListInstalls(ctx)
	if err != nil {
		return nil, err
	}
	byHost := make(map[string][]string)
	byID := make(map[string]bool)
	for _, install := range installs {
		byHost[install.Hostname] = append(byHost[install.Hostname], install.InstallID)
		byID[install.InstallID] = true
	}
	results := make([]metadataImportResult, len(rows))
	targetRows := make(map[string]int)
	for i, row := range rows {
		result := metadataImportResult{Row: i + 1, Hostname: row.Hostname, Candidates: byHost[row.Hostname]}
		switch {
		case row.InstallID != "":
			if byID[row.InstallID] {
				result.InstallID = row.InstallID
			} else {
				result.Status, result.Message = "unmatched", "Selected install does not belong to this organization"
			}
		case hostRows[row.Hostname] > 1:
			result.Status, result.Message = "ambiguous", "Multiple inventory rows share this hostname; select each install manually"
		case len(result.Candidates) == 0:
			result.Status, result.Message = "unmatched", "No install has this exact hostname; select an install manually"
		case len(result.Candidates) > 1:
			result.Status, result.Message = "ambiguous", "Multiple installs share this hostname; select an install manually"
		default:
			result.InstallID = result.Candidates[0]
		}
		if result.InstallID != "" {
			targetRows[result.InstallID]++
		}
		results[i] = result
	}
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
		err := m.PatchMetadata(ctx, result.InstallID, rows[i].Metadata)
		if err == nil {
			result.Status = "imported"
		} else {
			result.Status, result.Message = "error", "Metadata update unavailable; retry this row"
			if errors.Is(err, errInvalidMetadata) || errors.Is(err, ErrConflict) {
				result.Message = err.Error()
			}
		}
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
	results, err := manager.ImportMetadata(r.Context(), req.Rows)
	if err != nil {
		s.adminOperationError(w, "import install metadata", err)
		return
	}
	writeJSON(w, struct {
		Results []metadataImportResult `json:"results"`
	}{results})
}
