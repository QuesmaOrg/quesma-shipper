package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type adminConfigRequest struct {
	DisplayName             *string           `json:"display_name,omitempty"`
	AgeRecipients           []string          `json:"age_recipients"`
	IncludeInstallRecipient bool              `json:"include_install_recipient"`
	AllowQuesmaETL          *bool             `json:"allow_quesma_etl,omitempty"`
	AuthoredYAML            string            `json:"authored_yaml,omitempty"`
	Collection              *CollectionConfig `json:"collection,omitempty"`
	TelemetryCollectorURL   *string           `json:"telemetry_collector_url,omitempty"`
	// UnverifiedSources acknowledges, for this write only, source IDs no install reports. Never stored.
	UnverifiedSources []string `json:"unverified_sources,omitempty"`
}

// The collection alone: strict decoding refuses any other field, including one added to the config later.
type adminCollectionRequest struct {
	AuthoredYAML      string            `json:"authored_yaml,omitempty"`
	Collection        *CollectionConfig `json:"collection,omitempty"`
	UnverifiedSources []string          `json:"unverified_sources,omitempty"`
}

type adminCreateOrganizationRequest struct {
	Slug                    string            `json:"slug"`
	DisplayName             string            `json:"display_name"`
	AgeRecipients           []string          `json:"age_recipients"`
	IncludeInstallRecipient bool              `json:"include_install_recipient"`
	AllowQuesmaETL          *bool             `json:"allow_quesma_etl,omitempty"`
	AuthoredYAML            string            `json:"authored_yaml,omitempty"`
	Collection              *CollectionConfig `json:"collection,omitempty"`
	TelemetryCollectorURL   *string           `json:"telemetry_collector_url,omitempty"`
}

type adminExpiryRequest struct {
	ExpiresAt time.Time `json:"expires_at"`
}

type adminTagsRequest struct {
	Name      string `json:"name"`
	IfMissing bool   `json:"if_missing,omitempty"`
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
	return matchesDigest(credential, rec.Schema, rec.SecretDigest)
}

// VerifyPartnerCredential: a missing or revoked record authenticates nothing.
func (m *Manager) VerifyPartnerCredential(ctx context.Context, credential string) (bool, error) {
	rec, _, err := getRecord[PartnerCredentialRecord](ctx, m.store, partnerCredentialKey(m.org))
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ok, err := matchesDigest(credential, rec.Schema, rec.SecretDigest)
	return ok && rec.RevokedAt == nil, err
}

func matchesDigest(credential string, schema int, digest string) (bool, error) {
	if schema != schemaVersion {
		return false, errors.New("credential has unsupported schema")
	}
	want, err := hex.DecodeString(digest)
	if err != nil || len(want) != sha256.Size || digest != strings.ToLower(digest) {
		return false, errors.New("credential has invalid digest")
	}
	got := sha256.Sum256([]byte(credential))
	return subtle.ConstantTimeCompare(got[:], want) == 1, nil
}

func validAdminCredential(value string) bool { return wellFormedCredential(value, adminPrefix) }

func validReporterCredential(value string) bool { return wellFormedCredential(value, reporterPrefix) }

func validPartnerCredential(value string) bool { return wellFormedCredential(value, partnerPrefix) }

// The prefixes differ so a credential pasted into the wrong place is obvious at a glance, and so a
// reporter credential cannot be mistaken for administrative authority by anything reading a log.
const (
	adminPrefix    = "fma1."
	reporterPrefix = "fmr1."
	partnerPrefix  = "fmp1."
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
// route; a partner credential authorises the routes partnerRoute lists, in one organization.
type caller int

const (
	callerAdmin caller = iota
	callerReporter
	callerPartner
)

type principal struct {
	who caller
	org string // the partner's organization; empty for the deployment-wide credentials
}

type callerKey struct{}

func callerOf(r *http.Request) principal {
	if p, ok := r.Context().Value(callerKey{}).(principal); ok {
		return p
	}
	return principal{who: callerReporter}
}

// refuse names what the credential may do, so a 403 says which credential was presented.
func refuse(w http.ResponseWriter, who caller) {
	message := "this credential may only report health"
	if who == callerPartner {
		message = "this credential may not call this route"
	}
	writeAdminError(w, http.StatusForbidden, message)
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

		who, verify := principal{who: callerAdmin}, s.manager.VerifyAdminCredential
		switch {
		case validAdminCredential(credential):
		case validReporterCredential(credential):
			// Decided here rather than per route: this is the only place that knows the credential
			// is a reporter's, so a route added later is administrative by default. The opposite
			// default let a reporter read and rewrite organization configuration -- including its
			// age recipients, which is custody.
			if !reporterRoute(r) {
				refuse(w, callerReporter)
				return
			}
			who, verify = principal{who: callerReporter}, s.manager.VerifyReporterCredential
		case validPartnerCredential(credential):
			segments, ok := adminPathSegments(r)
			if !ok || len(segments) < 4 {
				refuse(w, callerPartner)
				return
			}
			// Looked up by the path's organization, so another organization's path fails as 401.
			manager, err := s.manager.ForOrganization(segments[3])
			if err != nil {
				writeAdminError(w, http.StatusUnauthorized, "invalid bearer authorization")
				return
			}
			who, verify = principal{who: callerPartner, org: segments[3]}, manager.VerifyPartnerCredential
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
		// After verification, unlike the reporter's, so another organization answers 401 on every route.
		if who.who == callerPartner && !partnerRoute(r) {
			refuse(w, callerPartner)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, who))
		w.Header().Del("WWW-Authenticate")
		if who.who != callerPartner || r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		// config.json records no author, so the log is where a partner's change can be told from an administrator's.
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		s.logger.Printf("organization %s: the partner credential's %s %s answered %d", who.org, r.Method, r.URL.EscapedPath(), recorder.status)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
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

// routeRegistrar is what registerOrganizationRoutes needs of a mux, so a test can list every route.
type routeRegistrar interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

func registerOrganizationRoutes(mux routeRegistrar, prefix string, s *Server) {
	mux.HandleFunc("GET "+prefix+"/config", s.partnerToo(s.handleAdminGetConfig))
	mux.HandleFunc("PUT "+prefix+"/config", s.adminOnly(s.handleAdminPutConfig))
	mux.HandleFunc("PUT "+prefix+"/collection", s.partnerToo(s.handleAdminPutCollection))
	mux.HandleFunc("GET "+prefix+"/partner", s.scopedAdmin(s.handleAdminGetPartner))
	mux.HandleFunc("POST "+prefix+"/partner", s.scopedAdmin(s.handleAdminMintPartner))
	mux.HandleFunc("POST "+prefix+"/partner/revoke", s.scopedAdmin(s.handleAdminRevokePartner))
	mux.HandleFunc("GET "+prefix+"/sources", s.scopedPartner(s.handleAdminListSources))
	mux.HandleFunc("GET "+prefix+"/grants", s.scopedPartner(s.handleAdminListGrants))
	mux.HandleFunc("POST "+prefix+"/grants", s.scopedPartner(s.handleAdminCreateGrant))
	mux.HandleFunc("POST "+prefix+"/grants/{id}/revoke", s.scopedPartner(s.handleAdminRevokeGrant))
	mux.HandleFunc("GET "+prefix+"/invites", s.scopedPartner(s.handleAdminListInvites))
	mux.HandleFunc("POST "+prefix+"/invites", s.scopedPartner(s.handleAdminCreateInvite))
	mux.HandleFunc("POST "+prefix+"/invites/{id}/revoke", s.scopedPartner(s.handleAdminRevokeInvite))
	mux.HandleFunc("POST "+prefix+"/invites/{id}/release", s.scopedPartner(s.handleAdminReleaseInvite))
	mux.HandleFunc("GET "+prefix+"/installs", s.scopedPartner(s.handleAdminListInstalls))
	mux.HandleFunc("GET "+prefix+"/installs/seen", s.scopedPartner(s.handleAdminListSeen))
	mux.HandleFunc("GET "+prefix+"/installs/health", s.scopedPartner(s.handleAdminListHealth))
	// The one route a reporter credential reaches, so it is not scopedAdmin. A partner reaches it too.
	mux.HandleFunc("POST "+prefix+"/installs/{id}/health", s.scoped(s.ownOrganization(s.handleAdminReportHealth)))
	mux.HandleFunc("GET "+prefix+"/installs/tags", s.scopedPartner(s.handleAdminListTags))
	mux.HandleFunc("PUT "+prefix+"/installs/{id}/tags", s.scopedPartner(s.handleAdminSetTag))
	mux.HandleFunc("PATCH "+prefix+"/installs/{id}/metadata", s.scopedPartner(s.handleAdminPatchMetadata))
	mux.HandleFunc("POST "+prefix+"/installs/metadata/import", s.scopedPartner(s.handleAdminImportMetadata))
	mux.HandleFunc("POST "+prefix+"/installs/{id}/revoke", s.scopedPartner(s.handleAdminRevokeInstall))
}

// Split as the mux matches, segment by escaped segment, so acme%2Fx is never read as acme.
func adminPathSegments(r *http.Request) ([]string, bool) {
	segments := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	for i, segment := range segments {
		unescaped, err := url.PathUnescape(segment)
		if err != nil {
			return nil, false
		}
		segments[i] = unescaped
	}
	ok := len(segments) >= 3 && segments[0] == "v1" && segments[1] == "admin" && segments[2] == "orgs"
	return segments, ok
}

// What a partner may call under /v1/admin/orgs/{slug}, as path.Match patterns.
var partnerRoutes = []string{
	"GET config", "GET sources", "GET grants", "GET invites",
	"GET installs", "GET installs/seen", "GET installs/health", "GET installs/tags",
	"PUT collection",
	"POST grants", "POST grants/*/revoke",
	"POST invites", "POST invites/*/revoke", "POST invites/*/release",
	"PUT installs/*/tags", "PATCH installs/*/metadata", "POST installs/metadata/import",
	"POST installs/*/revoke", "POST installs/*/health",
}

func partnerRoute(r *http.Request) bool {
	segments, ok := adminPathSegments(r)
	if !ok || len(segments) < 5 {
		return false
	}
	route := r.Method + " " + strings.Join(segments[4:], "/")
	for _, pattern := range partnerRoutes {
		if matched, _ := path.Match(pattern, route); matched {
			return true
		}
	}
	return false
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
		if who := callerOf(r).who; who != callerAdmin {
			refuse(w, who)
			return
		}
		next(w, r)
	}
}

func (s *Server) scopedAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.scoped(s.adminOnly(next))
}

// partnerToo admits an administrator, or a partner whose organization is the handler's.
func (s *Server) partnerToo(next http.HandlerFunc) http.HandlerFunc {
	return s.ownOrganization(func(w http.ResponseWriter, r *http.Request) {
		if who := callerOf(r).who; who != callerAdmin && who != callerPartner {
			refuse(w, who)
			return
		}
		next(w, r)
	})
}

func (s *Server) scopedPartner(next http.HandlerFunc) http.HandlerFunc {
	return s.scoped(s.partnerToo(next))
}

// ownOrganization refuses a partner whose organization is not the path's. Other callers pass.
func (s *Server) ownOrganization(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p := callerOf(r); p.who == callerPartner && p.org != r.PathValue("slug") {
			refuse(w, callerPartner)
			return
		}
		next(w, r)
	}
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
	switch any(out).(type) {
	case *adminConfigRequest, *adminCreateOrganizationRequest, *adminCollectionRequest:
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) == nil {
			for name, value := range fields {
				if !strings.EqualFold(name, "collection") {
					continue
				}
				if err := validateCollectionJSON(value); err != nil {
					writeAdminError(w, http.StatusBadRequest, err.Error())
					return false
				}
			}
		}
	}

	if err := strictDecode(body, out); err != nil {
		writeAdminError(w, http.StatusBadRequest, "malformed request: "+err.Error())
		return false
	}
	return true
}

func configFromAdminRequest(req adminConfigRequest) FleetConfig {
	cfg := FleetConfig{AgeRecipients: req.AgeRecipients, IncludeInstallRecipient: req.IncludeInstallRecipient,
		AllowQuesmaETL: req.AllowQuesmaETL, AuthoredYAML: req.AuthoredYAML, Collection: req.Collection, TelemetryCollectorURL: req.TelemetryCollectorURL}
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

// validateCollectionForFleet checks a new collection against what the organization's installs
// report they can execute. Only a write that states a collection is checked: one that keeps the
// stored collection must not be refused because the fleet moved since it was written.
func (s *Server) validateCollectionForFleet(w http.ResponseWriter, r *http.Request, manager *Manager, cfg FleetConfig, stored *CollectionConfig, unverified []string) bool {
	// The same normalization ApplyConfig runs, so authored_yaml is checked as the collection it becomes.
	cfg.Schema, cfg.Organization = schemaVersion, manager.org
	if err := normalizeCollection(&cfg); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return false
	}
	fleet, err := manager.FleetCatalog(r.Context())
	if err != nil {
		s.adminOperationError(w, "read install catalogs", err)
		return false
	}
	if message := validateCollectionAgainstFleet(cfg.Collection, stored, unverified, fleet); message != "" {
		writeAdminError(w, http.StatusBadRequest, message)
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
		AuthoredYAML: req.AuthoredYAML, Collection: req.Collection, TelemetryCollectorURL: req.TelemetryCollectorURL}
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
	// Keep old dashboard clients usable during rollout; durable records only contain collection.
	if cfg.Collection != nil {
		raw, _ := yaml.Marshal(cfg.Collection)
		cfg.AuthoredYAML = string(raw)
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

func requireConfigETag(w http.ResponseWriter, r *http.Request) (string, bool) {
	match := r.Header.Get("If-Match")
	if match == "" {
		writeAdminError(w, http.StatusPreconditionRequired, "If-Match is required")
		return "", false
	}
	version, ok := decodeETag(match)
	if !ok {
		writeAdminError(w, http.StatusBadRequest, "If-Match must contain the configuration ETag")
		return "", false
	}
	return version, true
}

func (s *Server) handleAdminPutConfig(w http.ResponseWriter, r *http.Request) {
	version, ok := requireConfigETag(w, r)
	if !ok {
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
	// Like the fields above: a client that does not mention collection must not erase it.
	if req.Collection == nil && req.AuthoredYAML == "" {
		cfg.Collection, cfg.AuthoredYAML = current.Collection, current.AuthoredYAML
	}
	if !s.validateAdminConfig(w, manager, cfg) {
		return
	}
	if req.Collection != nil || req.AuthoredYAML != "" {
		if !s.validateCollectionForFleet(w, r, manager, cfg, current.Collection, req.UnverifiedSources) {
			return
		}
	}
	s.applyConfig(w, r, manager, cfg, version)
}

func (s *Server) applyConfig(w http.ResponseWriter, r *http.Request, manager *Manager, cfg FleetConfig, version string) {
	err := manager.ApplyConfig(r.Context(), cfg, version)
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

// handleAdminPutCollection replaces the collection and keeps every other setting as stored.
func (s *Server) handleAdminPutCollection(w http.ResponseWriter, r *http.Request) {
	version, ok := requireConfigETag(w, r)
	if !ok {
		return
	}
	var req adminCollectionRequest
	if !readAdminJSON(w, r, &req) {
		return
	}
	if req.Collection == nil && req.AuthoredYAML == "" {
		writeAdminError(w, http.StatusBadRequest, "collection or authored_yaml is required")
		return
	}
	manager, err := s.adminManager(r)
	if err != nil {
		writeAdminError(w, http.StatusNotFound, "not found")
		return
	}
	current, _, err := manager.LoadConfig(r.Context())
	if errors.Is(err, ErrNotFound) {
		writeAdminError(w, http.StatusNotFound, "organization not found")
		return
	}
	if err != nil {
		s.adminOperationError(w, "load config", err)
		return
	}
	cfg := FleetConfig{DisplayName: current.DisplayName, AgeRecipients: current.AgeRecipients,
		IncludeInstallRecipient: current.IncludeInstallRecipient, AllowQuesmaETL: current.AllowQuesmaETL,
		TelemetryCollectorURL: current.TelemetryCollectorURL, Collection: req.Collection, AuthoredYAML: req.AuthoredYAML}
	if !s.validateAdminConfig(w, manager, cfg) || !s.validateCollectionForFleet(w, r, manager, cfg, current.Collection, req.UnverifiedSources) {
		return
	}
	s.applyConfig(w, r, manager, cfg, version)
}

// Whether the organization has a partner credential, without its digest.
func (s *Server) handleAdminGetPartner(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	rec, _, err := getRecord[PartnerCredentialRecord](r.Context(), manager.store, partnerCredentialKey(manager.org))
	if err != nil {
		s.adminOperationError(w, "read partner credential", err)
		return
	}
	writeJSON(w, struct {
		CreatedAt time.Time  `json:"created_at"`
		RevokedAt *time.Time `json:"revoked_at,omitempty"`
	}{rec.CreatedAt, rec.RevokedAt})
}

func (s *Server) handleAdminMintPartner(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	secret, err := manager.MintPartnerCredential(r.Context())
	if errors.Is(err, ErrConflict) {
		writeAdminError(w, http.StatusConflict, "a partner credential is active: revoke it before minting another")
		return
	}
	if err != nil {
		s.adminOperationError(w, "mint partner credential", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSONStatus(w, http.StatusCreated, adminSecretResponse{Secret: secret})
}

func (s *Server) handleAdminRevokePartner(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "revoke partner credential", manager.RevokePartnerCredential(r.Context()))
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

// handleAdminListSources is what the organization's active installs reported they can collect, so
// an editor offers only sources, rule packs and features some install can execute.
func (s *Server) handleAdminListSources(w http.ResponseWriter, r *http.Request) {
	manager, _ := s.adminManager(r)
	fleet, err := manager.FleetCatalog(r.Context())
	if err != nil {
		s.adminOperationError(w, "list sources", err)
		return
	}
	writeJSON(w, summarizeFleetCatalog(fleet))
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
	records, err := manager.ListTags(r.Context(), s.logger)
	s.writeAdminList(w, "list install tags", records, err)
}

func (s *Server) handleAdminSetTag(w http.ResponseWriter, r *http.Request) {
	var req adminTagsRequest
	if !readAdminJSON(w, r, &req) {
		return
	}
	manager, _ := s.adminManager(r)
	s.adminEmptyMutation(w, "set install name", manager.setTag(r.Context(), r.PathValue("id"), req.Name, req.IfMissing))
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
	case errors.Is(err, errInvalidMetadata):
		writeAdminError(w, http.StatusBadRequest, err.Error())
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
