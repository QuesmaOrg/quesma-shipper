package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

type adminConfigRequest struct {
	DisplayName             *string  `json:"display_name,omitempty"`
	AgeRecipients           []string `json:"age_recipients"`
	IncludeInstallRecipient bool     `json:"include_install_recipient"`
	AllowQuesmaETL          *bool    `json:"allow_quesma_etl,omitempty"`
	AuthoredYAML            string   `json:"authored_yaml"`
	TelemetryCollectorURL   *string  `json:"telemetry_collector_url,omitempty"`
}

type adminCreateOrganizationRequest struct {
	Slug                    string   `json:"slug"`
	DisplayName             string   `json:"display_name"`
	AgeRecipients           []string `json:"age_recipients"`
	IncludeInstallRecipient bool     `json:"include_install_recipient"`
	AllowQuesmaETL          *bool    `json:"allow_quesma_etl,omitempty"`
	AuthoredYAML            string   `json:"authored_yaml"`
	TelemetryCollectorURL   *string  `json:"telemetry_collector_url,omitempty"`
}

type adminExpiryRequest struct {
	ExpiresAt time.Time `json:"expires_at"`
}

type adminTagsRequest struct {
	Name string `json:"name"`
}

type adminSecretResponse struct {
	Secret string `json:"secret"`
}

type adminErrorResponse struct {
	Error string `json:"error"`
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeJSONStatus(w, status, adminErrorResponse{Error: message})
}

func (m *Manager) VerifyAdminCredential(ctx context.Context, credential string) (bool, error) {
	return m.verifyCredential(ctx, credential, deploymentAdminCredentialKey())
}

// VerifyReporterCredential checks the credential that may only report health. A deployment without
// one has no reporter, and every presented reporter credential fails -- which is the right default:
// the capability appears when an operator provisions it, not before.
func (m *Manager) VerifyReporterCredential(ctx context.Context, credential string) (bool, error) {
	return m.verifyCredential(ctx, credential, deploymentReporterCredentialKey())
}

func (m *Manager) verifyCredential(ctx context.Context, credential, key string) (bool, error) {
	rec, _, err := getRecord[AdminCredentialRecord](ctx, m.store, key)
	if errors.Is(err, ErrNotFound) {
		// Absent is not an error: a deployment provisions the reporter credential only if it wants
		// one, and until then nothing authenticates against it.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if rec.Schema != schemaVersion {
		return false, errors.New("credential has unsupported schema")
	}
	want, err := hex.DecodeString(rec.SecretDigest)
	if err != nil || len(want) != sha256.Size || rec.SecretDigest != strings.ToLower(rec.SecretDigest) {
		return false, errors.New("credential has invalid digest")
	}
	got := sha256.Sum256([]byte(credential))
	return subtle.ConstantTimeCompare(got[:], want) == 1, nil
}

func validAdminCredential(value string) bool { return wellFormedCredential(value, adminPrefix) }

func validReporterCredential(value string) bool { return wellFormedCredential(value, reporterPrefix) }

// The prefixes differ so a credential pasted into the wrong place is obvious at a glance, and so a
// reporter credential cannot be mistaken for administrative authority by anything reading a log.
const (
	adminPrefix    = "fma1."
	reporterPrefix = "fmr1."
)

func wellFormedCredential(value, prefix string) bool {
	secret, ok := strings.CutPrefix(value, prefix)
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(secret)
	}
	return err == nil && len(raw) == 32
}

// caller is what a request authenticated AS. Routes ask for what they need rather than trusting
// that authentication implies authority: the reporter credential authenticates, and authorises one
// route.
type caller int

const (
	callerAdmin caller = iota
	callerReporter
)

type callerKey struct{}

func callerOf(r *http.Request) caller {
	if who, ok := r.Context().Value(callerKey{}).(caller); ok {
		return who
	}
	return callerReporter
}

func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			writeAdminError(w, http.StatusUnauthorized, "bearer authorization required")
			return
		}
		scheme, credential, ok := strings.Cut(values[0], " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.ContainsAny(credential, " \t\r\n") {
			writeAdminError(w, http.StatusUnauthorized, "invalid bearer authorization")
			return
		}

		who, verify := callerAdmin, s.manager.VerifyAdminCredential
		switch {
		case validAdminCredential(credential):
		case validReporterCredential(credential):
			// Decided here rather than per route: this is the only place that knows the credential
			// is a reporter's, so a route added later is administrative by default. The opposite
			// default let a reporter read and rewrite organization configuration -- including its
			// age recipients, which is custody.
			if !reporterRoute(r) {
				writeAdminError(w, http.StatusForbidden, "this credential may only report health")
				return
			}
			who, verify = callerReporter, s.manager.VerifyReporterCredential
		default:
			writeAdminError(w, http.StatusUnauthorized, "invalid bearer authorization")
			return
		}

		verified, err := verify(r.Context(), credential)
		if err != nil {
			s.logger.Printf("admin authentication state read failed: %v", err)
			writeAdminError(w, http.StatusInternalServerError, "authentication unavailable")
			return
		}
		if !verified {
			writeAdminError(w, http.StatusUnauthorized, "invalid bearer authorization")
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, who))
		w.Header().Del("WWW-Authenticate")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/orgs", s.adminOnly(s.handleAdminListOrganizations))
	mux.HandleFunc("POST /v1/admin/orgs", s.adminOnly(s.handleAdminCreateOrganization))
	mux.HandleFunc("GET /v1/admin/defaults", s.adminOnly(s.handleAdminDefaults))
	registerOrganizationRoutes(mux, "/v1/admin/orgs/{slug}", s)
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		writeAdminError(w, http.StatusNotFound, "not found")
	}
	mux.HandleFunc("/v1/admin", notFound)
	mux.HandleFunc("/v1/admin/", notFound)
	return s.adminAuth(mux)
}

func registerOrganizationRoutes(mux *http.ServeMux, prefix string, s *Server) {
	mux.HandleFunc("GET "+prefix+"/config", s.adminOnly(s.handleAdminGetConfig))
	mux.HandleFunc("PUT "+prefix+"/config", s.adminOnly(s.handleAdminPutConfig))
	mux.HandleFunc("GET "+prefix+"/grants", s.scopedAdmin(s.handleAdminListGrants))
	mux.HandleFunc("POST "+prefix+"/grants", s.scopedAdmin(s.handleAdminCreateGrant))
	mux.HandleFunc("POST "+prefix+"/grants/{id}/revoke", s.scopedAdmin(s.handleAdminRevokeGrant))
	mux.HandleFunc("GET "+prefix+"/invites", s.scopedAdmin(s.handleAdminListInvites))
	mux.HandleFunc("POST "+prefix+"/invites", s.scopedAdmin(s.handleAdminCreateInvite))
	mux.HandleFunc("POST "+prefix+"/invites/{id}/revoke", s.scopedAdmin(s.handleAdminRevokeInvite))
	mux.HandleFunc("POST "+prefix+"/invites/{id}/release", s.scopedAdmin(s.handleAdminReleaseInvite))
	mux.HandleFunc("GET "+prefix+"/installs", s.scopedAdmin(s.handleAdminListInstalls))
	mux.HandleFunc("GET "+prefix+"/installs/seen", s.scopedAdmin(s.handleAdminListSeen))
	mux.HandleFunc("GET "+prefix+"/installs/health", s.scopedAdmin(s.handleAdminListHealth))
	// The one route a reporter credential reaches, so it is scoped rather than scopedAdmin.
	mux.HandleFunc("POST "+prefix+"/installs/{id}/health", s.scoped(s.handleAdminReportHealth))
	mux.HandleFunc("GET "+prefix+"/installs/tags", s.scopedAdmin(s.handleAdminListTags))
	mux.HandleFunc("PUT "+prefix+"/installs/{id}/tags", s.scopedAdmin(s.handleAdminSetTag))
	mux.HandleFunc("POST "+prefix+"/installs/{id}/revoke", s.scopedAdmin(s.handleAdminRevokeInstall))
}

// scopedAdmin is administrative authority. A reporter credential authenticates but is refused here,
// so adding a route without thinking about it denies the reporter rather than admitting it.
// reporterRoute is POST /v1/admin/orgs/{slug}/installs/{id}/health and nothing else.
func reporterRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	parts := strings.Split(strings.Trim(path.Clean(r.URL.Path), "/"), "/")
	return len(parts) == 7 && parts[0] == "v1" && parts[1] == "admin" && parts[2] == "orgs" &&
		parts[4] == "installs" && parts[6] == "health"
}

// adminOnly refuses a non-administrator without resolving anything first. The configuration routes
// use this rather than scopedAdmin because they resolve the organization themselves, and resolving
// it twice changes which failure an unreachable store reports.
func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if callerOf(r) != callerAdmin {
			writeAdminError(w, http.StatusForbidden, "this credential may only report health")
			return
		}
		next(w, r)
	}
}

func (s *Server) scopedAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.scoped(func(w http.ResponseWriter, r *http.Request) {
		if callerOf(r) != callerAdmin {
			writeAdminError(w, http.StatusForbidden, "this credential may only report health")
			return
		}
		next(w, r)
	})
}

// scoped resolves the organization without asking what the caller is.
func (s *Server) scoped(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		manager, err := s.adminManager(r)
		if err != nil {
			writeAdminError(w, http.StatusNotFound, "not found")
			return
		}
		if r.PathValue("slug") != "" {
			if _, _, err := manager.LoadConfig(r.Context()); errors.Is(err, ErrNotFound) {
				writeAdminError(w, http.StatusNotFound, "not found")
				return
			} else if err != nil {
				s.adminOperationError(w, "load organization", err)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) adminManager(r *http.Request) (*Manager, error) {
	org := r.PathValue("slug")
	if org == "" {
		return nil, errors.New("organization is required")
	}
	return s.manager.ForOrganization(org)
}

func readAdminJSON[T any](w http.ResponseWriter, r *http.Request, out *T) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, requestLimit+1))
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "read request")
		return false
	}
	if len(body) > requestLimit {
		writeAdminError(w, http.StatusBadRequest, "request exceeds 1 MiB")
		return false
	}
	if err := strictDecode(body, out); err != nil {
		writeAdminError(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return false
	}
	return true
}

func configFromAdminRequest(req adminConfigRequest) FleetConfig {
	cfg := FleetConfig{AgeRecipients: req.AgeRecipients, IncludeInstallRecipient: req.IncludeInstallRecipient,
		AllowQuesmaETL: req.AllowQuesmaETL, AuthoredYAML: req.AuthoredYAML, TelemetryCollectorURL: req.TelemetryCollectorURL}
	if req.DisplayName != nil {
		cfg.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	return cfg
}

func (s *Server) validateAdminConfig(w http.ResponseWriter, manager *Manager, cfg FleetConfig) bool {
	cfg.Schema, cfg.Organization = schemaVersion, manager.org
	if err := validateConfigForWrite(cfg); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func (s *Server) handleAdminListOrganizations(w http.ResponseWriter, r *http.Request) {
	records, err := s.manager.ListOrganizations(r.Context())
	s.writeAdminList(w, "list organizations", records, err)
}

func (s *Server) handleAdminCreateOrganization(w http.ResponseWriter, r *http.Request) {
	var req adminCreateOrganizationRequest
	if !readAdminJSON(w, r, &req) {
		return
	}
	if !orgPattern.MatchString(req.Slug) {
		writeAdminError(w, http.StatusBadRequest, "organization must be a lowercase slug of at most 63 characters")
		return
	}
	req.DisplayName = strings.TrimSpace(req.DisplayName)
	if err := validateDisplayName(req.DisplayName); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	manager, _ := s.manager.ForOrganization(req.Slug)
	cfg := FleetConfig{DisplayName: req.DisplayName, AgeRecipients: req.AgeRecipients,
		IncludeInstallRecipient: req.IncludeInstallRecipient, AllowQuesmaETL: req.AllowQuesmaETL,
		AuthoredYAML: req.AuthoredYAML, TelemetryCollectorURL: req.TelemetryCollectorURL}
	if !s.validateAdminConfig(w, manager, cfg) {
		return
	}
	if err := manager.Init(r.Context(), cfg); errors.Is(err, ErrConflict) {
		writeAdminError(w, http.StatusConflict, "organization already exists")
		return
	} else if err != nil {
		s.adminOperationError(w, "create organization", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, OrganizationSummary{Slug: req.Slug, DisplayName: req.DisplayName})
}

func encodeETag(version string) string {
	return `"` + base64.RawURLEncoding.EncodeToString([]byte(version)) + `"`
}

func decodeETag(value string) (string, bool) {
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[1 : len(value)-1])
	return string(raw), err == nil && len(raw) != 0
}

func (s *Server) handleAdminGetConfig(w http.ResponseWriter, r *http.Request) {
	manager, err := s.adminManager(r)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not found")
		return
	}
	cfg, version, err := manager.LoadConfig(r.Context())
	if errors.Is(err, ErrNotFound) {
		writeAdminError(w, http.StatusNotFound, "organization not found")
		return
	}
	if err != nil {
		s.adminOperationError(w, "load config", err)
		return
	}
	w.Header().Set("ETag", encodeETag(version))
	// The effective values, so the administration UI shows what installs are actually served
	// rather than leaving an unset setting to be guessed.
	writeJSON(w, s.defaults.resolve(cfg))
}

// handleAdminDefaults is what an organization created without a setting gets, so the UI can start
// its create form from the deployment's choice rather than from one written into the page.
func (s *Server) handleAdminDefaults(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.defaults)
}

func (s *Server) handleAdminPutConfig(w http.ResponseWriter, r *http.Request) {
	match := r.Header.Get("If-Match")
	if match == "" {
		writeAdminError(w, http.StatusPreconditionRequired, "If-Match is required")
		return
	}
	version, ok := decodeETag(match)
	if !ok {
		writeAdminError(w, http.StatusBadRequest, "If-Match must contain the configuration ETag")
		return
	}
	var req adminConfigRequest
	if !readAdminJSON(w, r, &req) {
		return
	}
	cfg := configFromAdminRequest(req)
	manager, err := s.adminManager(r)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not found")
		return
	}
	current, _, err := manager.LoadConfig(r.Context())
	if err != nil {
		s.adminOperationError(w, "load config", err)
		return
	}
	if req.TelemetryCollectorURL == nil {
		cfg.TelemetryCollectorURL = current.TelemetryCollectorURL
	}
	if req.DisplayName == nil {
		cfg.DisplayName = current.DisplayName
	}
	if req.AllowQuesmaETL == nil {
		cfg.AllowQuesmaETL = current.AllowQuesmaETL
	}
	if !s.validateAdminConfig(w, manager, cfg) {
		return
	}
	err = manager.ApplyConfig(r.Context(), cfg, version)
	if errors.Is(err, ErrConflict) {
		writeAdminError(w, http.StatusPreconditionFailed, "configuration changed")
		return
	}
	if err != nil {
		s.adminOperationError(w, "apply config", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdminListGrants(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	records, err := manager.ListGrants(r.Context())
	s.writeAdminList(w, "list grants", records, err)
}

func (s *Server) handleAdminCreateGrant(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	s.handleAdminCreateCredential(w, r, "create grant", manager.CreateGrant)
}

func (s *Server) handleAdminCreateCredential(w http.ResponseWriter, r *http.Request, operation string, create func(context.Context, time.Time) (string, error)) {
	var req adminExpiryRequest
	if !readAdminJSON(w, r, &req) {
		return
	}
	secret, err := create(r.Context(), req.ExpiresAt)
	if err != nil {
		s.adminOperationError(w, operation, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONStatus(w, http.StatusCreated, adminSecretResponse{Secret: secret})
}

func (s *Server) handleAdminRevokeGrant(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "revoke grant", manager.RevokeGrant(r.Context(), r.PathValue("id")))
}

func (s *Server) handleAdminListInvites(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	records, err := manager.ListInvites(r.Context())
	s.writeAdminList(w, "list invites", records, err)
}

func (s *Server) handleAdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	s.handleAdminCreateCredential(w, r, "create invite", manager.CreateInvite)
}

func (s *Server) handleAdminRevokeInvite(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "revoke invite", manager.RevokeInvite(r.Context(), r.PathValue("id")))
}

func (s *Server) handleAdminReleaseInvite(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "release invite", manager.ReleaseInvite(r.Context(), r.PathValue("id")))
}

func (s *Server) handleAdminListInstalls(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	records, err := manager.ListInstalls(r.Context())
	s.writeAdminList(w, "list installs", records, err)
}

// Telemetry is a separate request so the installs table renders from the identity records alone
// and fills in its detail columns once this fan-out lands.
func (s *Server) handleAdminListSeen(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	records, err := manager.ListSeen(r.Context())
	s.writeAdminList(w, "list install details", records, err)
}

// Health is listed separately from the identity records, like the telemetry: a caller renders
// its install table from the identities alone and joins health in when this lands. The admin UI
// no longer shows it; the dashboard does.
func (s *Server) handleAdminListHealth(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	records, err := manager.ListHealth(r.Context())
	s.writeAdminList(w, "list install health", records, err)
}

// handleAdminReportHealth accepts a report from whoever can read the archive. Fleet manager cannot
// read a heartbeat, so this is the only way collection health reaches it.
func (s *Server) handleAdminReportHealth(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	var report HealthReport
	if !readAdminJSON(w, r, &report) {
		return
	}
	if err := ValidateHealthReport(report); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	// An unknown or revoked install is the reporter naming something this organization does not
	// have, not a server fault: 404, so a stale client list is diagnosable from the response.
	err := manager.ReportHealth(r.Context(), r.PathValue("id"), report)
	if errors.Is(err, ErrUnknownInstall) || errors.Is(err, ErrRevokedInstall) {
		writeAdminError(w, http.StatusNotFound, "no such active install")
		return
	} else if err != nil {
		s.adminOperationError(w, "report install health", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeAdminList(w http.ResponseWriter, operation string, records any, err error) {
	if err != nil {
		s.adminOperationError(w, operation, err)
		return
	}
	writeJSON(w, records)
}

func (s *Server) handleAdminListTags(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	records, err := manager.ListTags(r.Context())
	s.writeAdminList(w, "list install names", records, err)
}

func (s *Server) handleAdminSetTag(w http.ResponseWriter, r *http.Request) {
	var req adminTagsRequest
	if !readAdminJSON(w, r, &req) {
		return
	}
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "set install name", manager.SetTag(r.Context(), r.PathValue("id"), req.Name))
}

func (s *Server) handleAdminRevokeInstall(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "revoke install", manager.RevokeInstall(r.Context(), r.PathValue("id")))
}

func (s *Server) adminEmptyMutation(w http.ResponseWriter, operation string, err error) {
	if err != nil {
		s.adminOperationError(w, operation, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminOperationError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeAdminError(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrConflict):
		writeAdminError(w, http.StatusConflict, "conflict")
	case err.Error() == "a spent invite cannot be released", err.Error() == "reservation install exists and is not revoked":
		writeAdminError(w, http.StatusConflict, err.Error())
	case err.Error() == "grant id is not a UUID", err.Error() == "invite id is not a UUID",
		err.Error() == "install id is not a UUID", err.Error() == "expiry must be in the future",
		strings.HasPrefix(err.Error(), "install name must "):
		writeAdminError(w, http.StatusBadRequest, err.Error())
	default:
		s.logger.Printf("admin %s failed: %v", operation, err)
		writeAdminError(w, http.StatusInternalServerError, operation+" unavailable")
	}
}
