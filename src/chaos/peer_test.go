//go:build chaos

// The far ends, in this process behind toxiproxy since MinIO cannot reset, hold or count PUTs on cue:
// a control plane that verifies device signatures, serves config and issues tickets, and a store that
// honours them. Each install carries its own fault, so parallel scenarios never share one.
package chaos

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
)

const (
	chaosOrg          = "acme"
	chaosBucket       = "chaos"
	authorizePreamble = "trajectory-shipper-upload-authorize-v2\nPOST\n/v2/uploads/authorize\n"
)

// Throwaway: it only lets the store tell a ticket it issued from a forged URL.
var ticketSecret = []byte("chaos-ticket-secret")

// How the control plane answers /v2/uploads/authorize for one install; the zero value is healthy.
type authorizeFault struct {
	status    int
	body      string
	stall     bool
	truncate  bool
	closeConn bool
}

// How the store answers a mirror-object PUT for one install; heartbeats always land.
type storeFault struct {
	resetAfterBody bool
	resetMidBody   int
	status         int
	body           string
	passFirst      int
	hold           *putHold
}

// A PUT that the store has read whole and then holds until the harness has cut the link around it.
type putHold struct {
	arrived chan struct{}
	release chan struct{}
}

// One authorize the control plane stalled, and how long the client waited before it gave up.
type stalledAuthorize struct {
	mirror bool
	waited time.Duration
}

type telemetryArrival struct {
	at      time.Time
	payload json.RawMessage
}

type install struct {
	id          string
	key         ed25519.PublicKey
	storeOrigin string

	mu         sync.Mutex
	authorize  authorizeFault
	store      storeFault
	onConfig   func()
	mirrorPuts int
	stalls     []stalledAuthorize
	telemetry  []telemetryArrival
	stored     map[string]int
}

type peers struct {
	mu       sync.Mutex
	installs map[string]*install
}

var far = &peers{installs: map[string]*install{}}

func (p *peers) register(id string, key ed25519.PublicKey, storeOrigin string) *install {
	in := &install{id: id, key: key, storeOrigin: storeOrigin, stored: map[string]int{}}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.installs[id] = in
	return in
}

func (p *peers) lookup(id string) *install {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.installs[id]
}

func (in *install) setAuthorize(f authorizeFault) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.authorize = f
}

// Restarts the PUT count passFirst reads, so a fault counts only the PUTs sent under it.
func (in *install) setStore(f storeFault) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.store, in.mirrorPuts = f, 0
}

func (in *install) setOnConfig(f func()) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.onConfig = f
}

func (in *install) mirrorObjects() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for key := range in.stored {
		if strings.Contains(key, "/mirror/") {
			n++
		}
	}
	return n
}

func (in *install) largestObject() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for _, size := range in.stored {
		n = max(n, size)
	}
	return n
}

func (in *install) stalled() []stalledAuthorize {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]stalledAuthorize(nil), in.stalls...)
}

// Every install_health payload that arrived in [from, to), oldest first.
func (in *install) telemetryBetween(from, to time.Time) []json.RawMessage {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []json.RawMessage
	for _, a := range in.telemetry {
		if !a.at.Before(from) && a.at.Before(to) {
			out = append(out, a.payload)
		}
	}
	return out
}

// --- the control plane ------------------------------------------------------

func (p *peers) serveControlPlane(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	org, id, sig, ok := parseDeviceAuthorization(r.Header.Get("Authorization"))
	in := p.lookup(id)
	if !ok || in == nil || org != chaosOrg {
		http.Error(w, "unknown install", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/v1/config":
		in.mu.Lock()
		hook := in.onConfig
		in.mu.Unlock()
		if hook != nil {
			hook()
		}
		writeJSON(w, map[string]any{
			"config": []byte("config_version: 1\nissued_at: \"2026-09-03T00:00:00Z\"\norg: " + chaosOrg +
				"\ntelemetry_endpoint: " + controlplane.TelemetryPath + "\n"),
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano),
		})
	case controlplane.TelemetryPath:
		if !ed25519.Verify(in.key, append([]byte(controlplane.TelemetryPreamble), body...), sig) {
			http.Error(w, "invalid device signature", http.StatusUnauthorized)
			return
		}
		var req controlplane.TelemetryRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		in.mu.Lock()
		in.telemetry = append(in.telemetry, telemetryArrival{at: time.Now(), payload: req.Payload})
		in.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	case "/v2/uploads/authorize":
		if !ed25519.Verify(in.key, append([]byte(authorizePreamble), body...), sig) {
			http.Error(w, "invalid device signature", http.StatusUnauthorized)
			return
		}
		p.authorize(w, r, in, body)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

type authorizeRequest struct {
	Objects []struct {
		ObjectID   string            `json:"object_id"`
		Key        string            `json:"key"`
		Size       int64             `json:"size"`
		SourceHash string            `json:"source_hash"`
		Metadata   map[string]string `json:"metadata"`
	} `json:"objects"`
}

func (p *peers) authorize(w http.ResponseWriter, r *http.Request, in *install, body []byte) {
	var req authorizeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mirror := len(req.Objects) > 0 && strings.Contains(req.Objects[0].Key, "/mirror/")
	in.mu.Lock()
	fault := in.authorize
	in.mu.Unlock()

	switch {
	case fault.stall:
		// Until the client gives up, bounded so a client that never does cannot wedge the suite.
		started := time.Now()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Minute):
		}
		in.mu.Lock()
		in.stalls = append(in.stalls, stalledAuthorize{mirror: mirror, waited: time.Since(started)})
		in.mu.Unlock()
		return
	case fault.status != 0:
		http.Error(w, fault.body, fault.status)
		return
	}

	tickets := make([]map[string]any, 0, len(req.Objects))
	for _, obj := range req.Objects {
		root := "v1/organization=" + chaosOrg + "/install=" + in.id + "/"
		if !strings.HasPrefix(obj.Key, root) {
			http.Error(w, fmt.Sprintf("object key %q is outside %q", obj.Key, root), http.StatusBadRequest)
			return
		}
		ticketID := uuid.NewString()
		headers := map[string]string{
			"x-amz-meta-source-hash": obj.SourceHash,
			"x-amz-meta-ticket-id":   ticketID,
			"x-amz-tagging":          "class=context",
		}
		if strings.Contains(obj.Key, "/mirror/") {
			headers["x-amz-tagging"] = "class=trajectory"
		}
		for name, value := range obj.Metadata {
			headers["x-amz-meta-"+name] = value
		}
		path := "/" + chaosBucket + "/" + canonicalPath(obj.Key)
		tickets = append(tickets, map[string]any{
			"ticket_id": ticketID, "object_id": obj.ObjectID, "method": http.MethodPut,
			"url":                   in.storeOrigin + path + "?sig=" + ticketSignature(path, obj.Size),
			"expires_at":            time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano),
			"required_headers":      headers,
			"content_length":        obj.Size,
			"content_length_signed": true,
		})
	}
	raw, err := json.Marshal(map[string]any{"tickets": tickets})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if fault.closeConn {
		w.Header().Set("Connection", "close")
	}
	if fault.truncate {
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw[:len(raw)/2])
		return
	}
	_, _ = w.Write(raw)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func parseDeviceAuthorization(header string) (org, install string, sig []byte, ok bool) {
	rest, found := strings.CutPrefix(header, "Shipper-Device ")
	if !found {
		return "", "", nil, false
	}
	values := map[string]string{}
	for field := range strings.SplitSeq(rest, ", ") {
		if name, value, ok := strings.Cut(field, "="); ok {
			values[name] = value
		}
	}
	sig, err := base64.StdEncoding.DecodeString(values["sig"])
	return values["org"], values["install"], sig, values["org"] != "" && values["install"] != "" && err == nil
}

func ticketSignature(path string, size int64) string {
	mac := hmac.New(sha256.New, ticketSecret)
	fmt.Fprintf(mac, "PUT\n%s\n%d", path, size)
	return hex.EncodeToString(mac.Sum(nil))
}

// The one AWS-style escaped spelling of a key that the shipper's exact-key check compares against.
func canonicalPath(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// --- the store --------------------------------------------------------------

func (p *peers) serveStore(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	if r.Method != http.MethodPut || !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(ticketSignature(path, r.ContentLength))) {
		http.Error(w, "<Error><Code>AccessDenied</Code></Error>", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/"+chaosBucket+"/")
	_, after, _ := strings.Cut(key, "/install=")
	id, _, _ := strings.Cut(after, "/")
	in := p.lookup(id)
	if in == nil {
		http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
		return
	}
	mirror := strings.Contains(key, "/mirror/")

	in.mu.Lock()
	fault := in.store
	nth := 0
	if mirror {
		in.mirrorPuts++
		nth = in.mirrorPuts
	}
	in.mu.Unlock()
	if !mirror || (fault.passFirst > 0 && nth <= fault.passFirst) {
		fault = storeFault{}
	}

	if fault.resetMidBody > 0 {
		_, _ = io.CopyN(io.Discard, r.Body, int64(fault.resetMidBody))
		resetConnection(w)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || int64(len(body)) != r.ContentLength {
		http.Error(w, "<Error><Code>IncompleteBody</Code></Error>", http.StatusBadRequest)
		return
	}
	switch {
	case fault.resetAfterBody:
		resetConnection(w)
		return
	case fault.status != 0:
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(fault.status)
		_, _ = io.WriteString(w, fault.body)
		return
	}
	if fault.hold != nil {
		// Never stored: the link is cut before the client could see an answer.
		fault.hold.arrived <- struct{}{}
		<-fault.hold.release
		w.WriteHeader(http.StatusOK)
		return
	}
	in.mu.Lock()
	in.stored[key] = len(body)
	in.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// Drops the TCP connection under the TLS one without a close_notify, so the client reads a reset
// rather than a clean end of stream wherever toxiproxy has armed SO_LINGER(0) on its side.
func resetConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	raw := conn
	if tc, ok := conn.(interface{ NetConn() net.Conn }); ok {
		raw = tc.NetConn()
	}
	if tcp, ok := raw.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = raw.Close()
}
