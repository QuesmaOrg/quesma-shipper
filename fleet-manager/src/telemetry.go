package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

const telemetryShipperPreamble = "trajectory-shipper-telemetry-v1\nPOST\n/v1/telemetry\n"
const telemetryFleetNamespace = "quesma-fleet-telemetry-v1\nPOST\n"

type telemetryRequest struct {
	Schema   int             `json:"schema"`
	BatchID  string          `json:"batch_id"`
	IssuedAt time.Time       `json:"issued_at"`
	Payload  json.RawMessage `json:"payload"`
}

type forwardedTelemetry struct {
	Schema          int       `json:"schema"`
	FleetManagerID  string    `json:"fleet_manager_id"`
	Organization    string    `json:"organization"`
	InstallID       string    `json:"install_id"`
	BatchID         string    `json:"batch_id"`
	ForwardedAt     time.Time `json:"forwarded_at"`
	Audience        string    `json:"audience"`
	ShipperEnvelope []byte    `json:"shipper_envelope"`
}

type telemetryBucket struct {
	tokens  float64
	updated time.Time
}

type telemetryProxy struct {
	identity    telemetryRegistration
	key         ed25519.PrivateKey
	client      *http.Client
	timeout     time.Duration
	slots       chan struct{}
	rate, burst int
	mu          sync.Mutex
	buckets     map[string]telemetryBucket
}

func newTelemetryProxy(identity telemetryIdentity, key ed25519.PrivateKey, concurrency, rate, burst int) *telemetryProxy {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = concurrency
	transport.MaxIdleConnsPerHost = concurrency
	// Custom destinations must not leave idle connections to an unbounded set of hosts.
	transport.IdleConnTimeout = 30 * time.Second
	return &telemetryProxy{
		identity: registration(identity.FleetManagerID, key), key: key,
		client:  &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		timeout: 5 * time.Second, slots: make(chan struct{}, concurrency), rate: rate, burst: burst,
		buckets: make(map[string]telemetryBucket),
	}
}

func (p *telemetryProxy) allow(id string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	bucket, exists := p.buckets[id]
	if !exists {
		if len(p.buckets) >= 10000 {
			refill := time.Duration(float64(p.burst) / float64(p.rate) * float64(time.Minute))
			for key, old := range p.buckets {
				if now.Sub(old.updated) >= refill {
					delete(p.buckets, key)
				}
			}
			if len(p.buckets) >= 10000 {
				return false
			}
		}
		bucket = telemetryBucket{tokens: float64(p.burst), updated: now}
	}
	elapsed := now.Sub(bucket.updated).Minutes()
	if elapsed > 0 {
		bucket.tokens = min(float64(p.burst), bucket.tokens+elapsed*float64(p.rate))
		bucket.updated = now
	}
	allowed := bucket.tokens >= 1
	if allowed {
		bucket.tokens--
	}
	p.buckets[id] = bucket
	return allowed
}

// Admission bounds concurrent request buffers as well as upstream work, including unknown devices.
func (s *Server) telemetryAdmission(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		select {
		case s.telemetry.slots <- struct{}{}:
			defer func() { <-s.telemetry.slots }()
		default:
			w.Header().Set("Retry-After", "1")
			writeJSONError(w, http.StatusTooManyRequests, "telemetry_busy")
			return
		}
		// Use a deadline even for incoming bodies so slow senders cannot retain every slot.
		deadline := time.Now().Add(5 * time.Second)
		_ = http.NewResponseController(w).SetReadDeadline(deadline)
		defer func() { _ = http.NewResponseController(w).SetReadDeadline(time.Time{}) }()
		raw, tooLarge, err := readCapped(r.Body, requestLimit)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "telemetry_read_failed")
			return
		}
		if tooLarge {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "telemetry_too_large")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		next(w, r)
	}
}

func (s *Server) handleTelemetry(w http.ResponseWriter, r *http.Request, rec InstallRecord, body []byte) {
	scoped, _ := s.manager.ForOrganization(rec.Organization)
	cfg, _, err := scoped.LoadConfig(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "telemetry_unavailable")
		return
	}
	cfg = s.defaults.resolve(cfg)
	if cfg.telemetryCollectorURL() == "" {
		writeJSONError(w, http.StatusForbidden, "telemetry_disabled")
		return
	}
	p := s.telemetry
	if !p.allow(rec.Organization+"/"+rec.InstallID, time.Now()) {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, (60+p.rate-1)/p.rate)))
		writeJSONError(w, http.StatusTooManyRequests, "telemetry_rate_limited")
		return
	}
	var request telemetryRequest
	if err := strictDecode(body, &request); err != nil || request.Schema != 1 || len(request.Payload) == 0 {
		writeJSONError(w, http.StatusBadRequest, "telemetry_invalid_request")
		return
	}
	id, err := uuid.Parse(request.BatchID)
	if err != nil || id == uuid.Nil || id.String() != request.BatchID {
		writeJSONError(w, http.StatusBadRequest, "telemetry_invalid_batch_id")
		return
	}
	now := s.manager.time()
	if request.IssuedAt.Before(now.Add(-5*time.Minute)) || request.IssuedAt.After(now.Add(time.Minute)) {
		writeJSONError(w, http.StatusBadRequest, "telemetry_stale_request")
		return
	}
	endpoint, err := normalizeTelemetryCollectorURL(cfg.telemetryCollectorURL())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "telemetry_unavailable")
		return
	}
	envelope := forwardedTelemetry{Schema: 1, FleetManagerID: p.identity.FleetManagerID,
		Organization: rec.Organization, InstallID: rec.InstallID, BatchID: request.BatchID,
		ForwardedAt: now, Audience: endpoint, ShipperEnvelope: body}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "telemetry_encoding_failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "telemetry_unavailable")
		return
	}
	signature := ed25519.Sign(p.key, append([]byte(telemetryFleetPreamble(req.URL)), encoded...))
	req.Header.Set("Authorization", "Fleet-Telemetry key="+p.identity.KeyID+",sig="+base64.StdEncoding.EncodeToString(signature))
	req.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(req)
	if err != nil {
		status, code := http.StatusBadGateway, "telemetry_upstream_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			status, code = http.StatusGatewayTimeout, "telemetry_upstream_timeout"
		}
		writeJSONError(w, status, code)
		s.logger.Printf("telemetry forwarding failed: %s", code)
		return
	}
	defer response.Body.Close()
	// Upstream bodies are never relayed or logged; cap draining to keep memory and time bounded.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.logger.Printf("telemetry collector rejected request: status=%d", response.StatusCode)
		if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusConflict || response.StatusCode == http.StatusRequestEntityTooLarge || response.StatusCode == http.StatusUnprocessableEntity {
			writeJSONError(w, response.StatusCode, "telemetry_rejected")
			return
		}
		writeJSONError(w, http.StatusBadGateway, "telemetry_upstream_failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func telemetryFleetPreamble(u *url.URL) string {
	return telemetryFleetNamespace + u.EscapedPath() + "\n"
}
