package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

const (
	requestLimit              = 1 << 20
	stateObjectLimit          = 4 << 20
	uploadAuthorizePreamble   = "trajectory-shipper-upload-authorize-v2\nPOST\n/v2/uploads/authorize\n"
	domainSeparationNamespace = "trajectory-shipper-"
)

type Server struct {
	manager   *Manager
	signer    UploadSigner
	configTTL time.Duration
	logger    *log.Logger
	telemetry *telemetryProxy
	// defaults fills the settings an organization never stated, wherever one is used.
	defaults OrganizationDefaults
}

func NewServer(manager *Manager, signer UploadSigner, logger *log.Logger) (*Server, error) {
	if manager == nil || signer == nil {
		return nil, errors.New("manager and upload signer are required")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Server{manager: manager, signer: signer, configTTL: 24 * time.Hour, logger: logger, defaults: defaultOrganizationDefaults()}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	admin := s.adminHandler()
	health := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	}
	mux.HandleFunc("GET /healthz", health)
	// Cloud Run reserves /healthz at its frontend, so its deployment uses this alias.
	mux.HandleFunc("GET /health", health)
	mux.HandleFunc("POST /v1/enroll", s.handleEnroll)
	mux.HandleFunc("GET /v1/telemetry/public-key", s.handleTelemetryPublicKey)
	mux.HandleFunc("POST /v1/telemetry", s.telemetryAdmission(s.deviceAuth(telemetryShipperPreamble, s.handleTelemetry)))
	mux.HandleFunc("POST /v1/config", s.deviceAuth("", s.handleConfig))
	mux.HandleFunc("POST /v2/uploads/authorize", s.deviceAuth(uploadAuthorizePreamble, s.handleUploadAuthorize))
	mux.Handle("/v1/admin", admin)
	mux.Handle("/v1/admin/", admin)
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	mux.Handle("/admin/", adminUIHandler())
	return mux
}

type enrollRequest struct {
	Invite          string `json:"invite,omitempty"`
	Grant           string `json:"grant,omitempty"`
	InstallID       string `json:"install_id"`
	DevicePublicKey string `json:"device_public_key"`
	AgeRecipient    string `json:"age_recipient"`
	Hostname        string `json:"hostname,omitempty"`
	Platform        string `json:"platform,omitempty"`
}
type enrollResponse struct {
	Organization string `json:"organization"`
}
type configRequest struct {
	AgentVersion   string `json:"agent_version"`
	ConfigVersions []int  `json:"config_versions"`
}
type configResponse struct {
	Config    []byte    `json:"config"`
	ExpiresAt time.Time `json:"expires_at"`
}

// readCapped reads up to limit bytes, and one more to tell a body at the limit from one past it.
func readCapped(r io.Reader, limit int64) (body []byte, tooLarge bool, err error) {
	body, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	return body, int64(len(body)) > limit, nil
}

func readRequest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, tooLarge, err := readCapped(r.Body, requestLimit)
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return nil, false
	}
	if tooLarge {
		http.Error(w, "request exceeds 1 MiB", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	body, ok := readRequest(w, r)
	if !ok {
		return
	}
	var req enrollRequest
	if err := strictDecode(body, &req); err != nil {
		http.Error(w, "malformed enroll request: "+err.Error(), http.StatusBadRequest)
		return
	}
	parsedID, err := uuid.Parse(req.InstallID)
	if err != nil {
		http.Error(w, "install_id is not a UUID", http.StatusBadRequest)
		return
	}
	if parsedID.String() != strings.ToLower(req.InstallID) {
		req.InstallID = parsedID.String()
	}
	if _, err := age.ParseX25519Recipient(req.AgeRecipient); err != nil {
		http.Error(w, "age_recipient does not parse as an age X25519 recipient", http.StatusBadRequest)
		return
	}
	pub, err := base64.StdEncoding.DecodeString(req.DevicePublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		http.Error(w, "device_public_key must be a base64 32-byte ed25519 public key", http.StatusBadRequest)
		return
	}
	if (req.Invite == "") == (req.Grant == "") {
		http.Error(w, "pass exactly one of invite or grant", http.StatusBadRequest)
		return
	}
	rec := InstallRecord{InstallID: req.InstallID, DevicePublicKey: req.DevicePublicKey, AgeRecipient: req.AgeRecipient,
		Hostname: req.Hostname, Platform: req.Platform, EnrollmentDigest: enrollmentDigest(body)}
	credential := req.Invite
	if req.Grant != "" {
		credential = req.Grant
	}
	org, err := enrollmentOrganization(credential)
	if err == nil {
		var scoped *Manager
		scoped, err = s.manager.ForOrganization(org)
		if err == nil && req.Grant != "" {
			err = scoped.EnrollGrant(r.Context(), req.Grant, rec)
		} else if err == nil {
			err = scoped.EnrollInvite(r.Context(), req.Invite, rec)
		}
	}
	switch {
	case err == nil:
		s.touch(r, InstallRecord{Organization: org, InstallID: req.InstallID}, seenEnrollment)
		writeJSON(w, enrollResponse{Organization: org})
	case errors.Is(err, ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrEnrollmentConflict), errors.Is(err, ErrConflict):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		s.logger.Printf("enrollment failed for install %s: %v", req.InstallID, err)
		http.Error(w, "enrollment unavailable", http.StatusInternalServerError)
	}
}

type authenticatedHandler func(http.ResponseWriter, *http.Request, InstallRecord, []byte)

func (s *Server) deviceAuth(preamble string, next authenticatedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if preamble != "" {
			w.Header().Set("Cache-Control", "no-store")
		}
		body, ok := readRequest(w, r)
		if !ok {
			return
		}
		if preamble == "" && bytes.HasPrefix(body, []byte(domainSeparationNamespace)) {
			http.Error(w, "body is not a v1 request", http.StatusBadRequest)
			return
		}
		auth := r.Header.Get("Authorization")
		const scheme = "Shipper-Device "
		if !strings.HasPrefix(auth, scheme) {
			http.Error(w, "missing Shipper-Device authorization", http.StatusUnauthorized)
			return
		}
		var org, installID, signature string
		for _, field := range strings.Split(strings.TrimPrefix(auth, scheme), ",") {
			key, value, found := strings.Cut(strings.TrimSpace(field), "=")
			if !found {
				continue
			}
			switch key {
			case "org":
				org = value
			case "install":
				installID = value
			case "sig":
				signature = value
			}
		}
		org, orgErr := deviceOrganization(org)
		if _, err := uuid.Parse(installID); err != nil || signature == "" || orgErr != nil {
			http.Error(w, "invalid Shipper-Device authorization", http.StatusUnauthorized)
			return
		}
		scoped, _ := s.manager.ForOrganization(org)
		rec, err := scoped.LoadActiveInstall(r.Context(), installID)
		if errors.Is(err, ErrUnknownInstall) {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		if errors.Is(err, ErrRevokedInstall) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		if err != nil {
			s.logger.Printf("device authentication state read failed for install %s: %v", installID, err)
			http.Error(w, "authentication unavailable", http.StatusInternalServerError)
			return
		}
		pub, err := base64.StdEncoding.DecodeString(rec.DevicePublicKey)
		sig, sigErr := base64.StdEncoding.DecodeString(signature)
		if err != nil || sigErr != nil || len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, append([]byte(preamble), body...), sig) {
			http.Error(w, "signature does not verify", http.StatusUnauthorized)
			return
		}
		next(w, r, rec, body)
	}
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, rec InstallRecord, body []byte) {
	var req configRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "malformed config request", http.StatusBadRequest)
		return
	}
	if req.AgentVersion == "" || len(req.ConfigVersions) == 0 {
		http.Error(w, "agent_version and config_versions are required", http.StatusBadRequest)
		return
	}
	if !slices.Contains(req.ConfigVersions, 1) {
		http.Error(w, "this service serves config_version 1 only", http.StatusConflict)
		return
	}
	scoped, _ := s.manager.ForOrganization(rec.Organization)
	cfg, _, err := scoped.LoadConfig(r.Context())
	if err != nil {
		s.logger.Printf("config state read failed for install %s: %v", rec.InstallID, err)
		http.Error(w, "config unavailable", http.StatusInternalServerError)
		return
	}
	s.touch(r, rec, seenConfig)
	cfg = s.defaults.resolve(cfg)
	doc := renderConfig(cfg)
	endpoint := ""
	if cfg.telemetryCollectorURL() != "" {
		endpoint = telemetryPath
	}
	field, _ := yaml.Marshal(map[string]string{"telemetry_endpoint": endpoint})
	doc += string(field)
	writeJSON(w, configResponse{Config: []byte(doc), ExpiresAt: s.manager.time().Add(s.configTTL)})
}

func renderConfig(cfg FleetConfig) string {
	var out strings.Builder
	fmt.Fprintf(&out, "config_version: 1\nissued_at: %s\norg: %s\nencryption:\n  additional_recipients:\n", cfg.UpdatedAt.UTC().Format(time.RFC3339), cfg.Organization)
	for _, recipient := range cfg.AgeRecipients {
		fmt.Fprintf(&out, "    - %s\n", recipient)
	}
	if cfg.quesmaETLEnabled() {
		fmt.Fprintf(&out, "    - %s\n", quesmaETLAgeRecipient)
	}
	if !cfg.IncludeInstallRecipient {
		out.WriteString("  include_install_recipient: false\n")
	}
	if authored := strings.TrimSpace(cfg.AuthoredYAML); authored != "" {
		out.WriteString(authored)
		out.WriteByte('\n')
	}
	return out.String()
}

func (s *Server) handleUploadAuthorize(w http.ResponseWriter, r *http.Request, rec InstallRecord, body []byte) {
	w.Header().Set("Cache-Control", "no-store")
	var req uploadAuthorizeRequest
	if err := strictDecode(body, &req); err != nil {
		http.Error(w, "malformed upload authorization request: "+err.Error(), http.StatusBadRequest)
		return
	}
	now := s.manager.time()
	objects, err := validateUploadRequest(req, rec, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for i := range objects {
		objects[i].TicketID = uuid.NewString()
		objects[i].Metadata["ticket-id"] = objects[i].TicketID
	}
	batch := UploadBatch{IssuedAt: now, ExpiresAt: now.Add(uploadTicketLifetime), Objects: objects}
	var settled []uploadTicket
	batch.Objects, settled = splitAlreadyStored(r.Context(), s.manager.store, s.logger, rec.InstallID, batch.Objects)
	// A batch the store already holds whole leaves nothing to sign.
	var tickets TicketBatch
	if len(batch.Objects) > 0 {
		var err error
		tickets, err = s.signer.Authorize(r.Context(), InstallScope{Organization: rec.Organization, InstallID: rec.InstallID}, batch)
		if err != nil {
			s.logger.Printf("upload authorization failed for install %s: %s", rec.InstallID, scrubURLs(err.Error()))
			http.Error(w, "upload authorization failed", http.StatusInternalServerError)
			return
		}
		if err := validateTicketBatch(batch, tickets); err != nil {
			s.logger.Printf("upload signer returned invalid tickets for install %s: %s", rec.InstallID, scrubURLs(err.Error()))
			http.Error(w, "upload authorization failed", http.StatusInternalServerError)
			return
		}
	}
	s.touch(r, rec, seenVend)
	writeJSON(w, uploadAuthorizeResponse{Tickets: append(tickets.Tickets, settled...)})
}

func validateTicketBatch(batch UploadBatch, response TicketBatch) error {
	if len(response.Tickets) != len(batch.Objects) {
		return errors.New("ticket count does not match request")
	}
	objects := make(map[string]UploadObjectRequest, len(batch.Objects))
	for _, object := range batch.Objects {
		objects[object.ObjectID] = object
	}
	seen := map[string]bool{}
	for _, ticket := range response.Tickets {
		object, ok := objects[ticket.ObjectID]
		if !ok || seen[ticket.ObjectID] {
			return errors.New("ticket object_id is unknown or duplicated")
		}
		seen[ticket.ObjectID] = true
		if ticket.TicketID != object.TicketID || ticket.Method != http.MethodPut || ticket.ContentLength != object.Size || !ticket.ContentLengthSigned {
			return errors.New("ticket does not preserve the requested capability")
		}
		if ticket.ExpiresAt.After(batch.ExpiresAt) || !ticket.ExpiresAt.After(batch.IssuedAt) {
			return errors.New("ticket expiry is outside the authorization window")
		}
		u, err := url.Parse(ticket.URL)
		if err != nil || u.Host == "" || u.User != nil || !ticketSchemeAllowed(u) {
			return errors.New("ticket URL is not an https origin, nor a loopback http one")
		}
		if !strings.HasSuffix(u.Path, "/"+object.Key) {
			return errors.New("ticket URL does not name the exact requested key")
		}
		if err := validateTicketHeaders(object, ticket.RequiredHeaders); err != nil {
			return err
		}
	}
	return nil
}

// Loopback http is the one exemption, and it is what makes a store on the same machine — MinIO in
// a local stack — reachable at all: bytes addressed to loopback cannot leave it, so there is no
// transport to protect. Everything else must be https. The client still refuses such a ticket
// unless its own upload_targets pin admits loopback http, so this widens nothing on its own.
func ticketSchemeAllowed(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var ticketHeaderDialects = []struct{ prefix, dialect string }{
	{"x-amz-", "aws"}, {"x-goog-", "gcp"}, {"x-ms-", "azure"},
}

func ticketHeaderDialect(name string) string {
	for _, d := range ticketHeaderDialects {
		if strings.HasPrefix(name, d.prefix) {
			return d.dialect
		}
	}
	return ""
}

// validateTicketHeaders rebuilds the expected headers itself rather than calling the signers'
// builders, so it checks them instead of agreeing with them.
func validateTicketHeaders(object UploadObjectRequest, headers map[string]string) error {
	if len(headers) == 0 {
		return errors.New("ticket carries no required headers")
	}
	var dialect string
	for name := range headers {
		if name != strings.ToLower(name) {
			return errors.New("ticket header names must be lowercase")
		}
		d := ticketHeaderDialect(name)
		if d == "" {
			return fmt.Errorf("ticket header %q is outside the protocol", name)
		}
		if dialect != "" && dialect != d {
			return errors.New("ticket mixes provider header dialects")
		}
		dialect = d
	}
	want := map[string]string{}
	for name, value := range object.Metadata {
		switch dialect {
		case "aws":
			want["x-amz-meta-"+strings.ToLower(name)] = value
		case "gcp":
			want["x-goog-meta-"+strings.ToLower(name)] = value
		case "azure":
			want["x-ms-meta-"+strings.ReplaceAll(strings.ToLower(name), "-", "_")] = value
		}
	}
	switch dialect {
	case "aws":
		if object.Tagging != "" {
			want["x-amz-tagging"] = object.Tagging
		}
	case "azure":
		want["x-ms-blob-type"] = "BlockBlob"
		if object.Tagging != "" {
			want["x-ms-tags"] = object.Tagging
		}
	}
	if len(headers) != len(want) {
		return errors.New("ticket required headers do not exactly match the validated object")
	}
	for name, value := range want {
		if headers[name] != value {
			return fmt.Errorf("ticket header %q does not match the validated object", name)
		}
	}
	return nil
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSONStatus(w, status, errorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, value any) {
	writeJSONStatus(w, http.StatusOK, value)
}

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}
