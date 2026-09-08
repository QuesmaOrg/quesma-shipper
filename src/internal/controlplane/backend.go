// Package controlplane is the optional control-plane client: with no endpoint configured it is
// never constructed and the build makes zero network calls. It offers enroll, config and upload
// authorization, and must never offer an upload-status endpoint, a cursor API or per-object acks.
package controlplane

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// maxResponseBytes bounds what the client will read: a large response is either a bug or a
// hostile server trying to exhaust memory.
const maxResponseBytes = 4 << 20

// defaultTimeout keeps a hung control plane from stalling collection: a backend that never
// answers costs one timeout rather than a tick.
const defaultTimeout = 30 * time.Second

// VersionHeader carries the client version on every request. A header, not a body field: the
// server must know which build is talking even on a request it is about to refuse.
const VersionHeader = "X-Shipper-Version"

// OSHeader and BootHeader follow the same contract as VersionHeader: best-effort
// machine facts ("macOS 26.5.1", boot as RFC3339) recorded per install.
const (
	OSHeader   = "X-Shipper-OS"
	BootHeader = "X-Shipper-Boot"
)

var (
	// ErrNotEnrolled means no enrollment record exists locally.
	ErrNotEnrolled = errors.New("backend: this install is not enrolled")

	// ErrUnsupportedVersion is the 409 case: the server has no config this client can execute
	// and says so, instead of serving one that would partly apply.
	ErrUnsupportedVersion = errors.New("backend: server has no config this client can execute")

	// ErrAuthorizeUnavailable is authorize answering 429 or 5xx: the batch commits nothing and a
	// later run retries it. No in-run retry, no backoff loop.
	ErrAuthorizeUnavailable = errors.New("backend: upload authorization is unavailable")
)

// Client talks to the control plane.
type Client struct {
	endpoint string
	http     *http.Client

	installID    string
	organization string
	deviceKey    ed25519.PrivateKey
}

// Options configures the client.
type Options struct {
	Endpoint     string
	InstallID    string
	Organization string

	// DeviceKey signs requests after enrollment, verified against the public key in the record the
	// server reads fresh per request, so a client cannot present a scope it was not granted.
	DeviceKey ed25519.PrivateKey

	HTTPClient *http.Client
}

// New builds a client. An empty endpoint is an error rather than a no-op: that caller has a bug.
func New(o Options) (*Client, error) {
	if o.Endpoint == "" {
		return nil, errors.New("backend: no endpoint configured")
	}
	httpClient := o.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: defaultTimeout,

			// A control-plane call is never a redirect: following one would strip the device
			// signature or replay it against a host the operator never named.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("backend: refusing redirect to %s", req.URL.Host)
			},
		}
	}
	return &Client{
		endpoint:     strings.TrimSuffix(o.Endpoint, "/"),
		http:         httpClient,
		installID:    o.InstallID,
		organization: o.Organization,
		deviceKey:    o.DeviceKey,
	}, nil
}

// ConfigRequest is what a config fetch posts: which build is asking and which config schema
// versions it can execute. Not a min-version pin: a floor can brick a fleet that cannot move.
type ConfigRequest struct {
	AgentVersion   string `json:"agent_version"`
	ConfigVersions []int  `json:"config_versions"`
}

// EnrollRequest is the one-time registration.
type EnrollRequest struct {
	// Invite is a server-minted claim, bounded expiry and single use. An identical completed
	// enrollment retry succeeds; changed enrollment material is refused.
	Invite string `json:"invite,omitempty"`

	// Grant is the managed-enrollment claim: multi-use, opaque here, and exactly one of Invite or
	// Grant is set. omitempty keeps invite enrollments byte-identical for servers predating grants.
	Grant string `json:"grant,omitempty"`

	InstallID string `json:"install_id"`

	// DevicePublicKey is what later requests are verified against.
	DevicePublicKey string `json:"device_public_key"`

	// AgeRecipient lets the org add this install as a recipient: the PUBLIC half only.
	AgeRecipient string `json:"age_recipient"`

	Hostname string `json:"hostname,omitempty"`
	Platform string `json:"platform,omitempty"`
}

// EnrollResponse is what the server returns once.
type EnrollResponse struct {
	// Organization is server-assigned: an install cannot place itself in another org's subtree.
	Organization string `json:"organization"`
}

// ConfigResponse carries ONE document: the org's config, trusted over TLS from the enrolled plane.
type ConfigResponse struct {
	// Config is the served YAML verbatim, parsed tolerantly: the schema moves ahead of the fleet.
	Config []byte `json:"config"`

	// ExpiresAt lets the client keep collecting on a cached config and stamp config_expired rather
	// than halting, which would lose data: source stores are reaped on their own schedules.
	ExpiresAt time.Time `json:"expires_at"`
}

// Enroll registers this install.
func (c *Client) Enroll(ctx context.Context, req EnrollRequest) (*EnrollResponse, error) {
	var out EnrollResponse
	if err := c.post(ctx, "/v1/enroll", req, &out, false); err != nil {
		return nil, err
	}
	if out.Organization == "" {
		return nil, errors.New("backend: enrollment returned no organization")
	}
	return &out, nil
}

// Fetched is one config fetch. The raw bytes come back beside the parsed document because the
// cache stores them verbatim: re-encoding would drop every field this build does not know.
type Fetched struct {
	Doc *config.Document
	Raw []byte

	ExpiresAt time.Time
}

// FetchConfig posts the config request and returns the served config. A document that does not
// parse is refused here rather than downstream: a config that partly applied is worse than none.
func (c *Client) FetchConfig(ctx context.Context, req ConfigRequest) (Fetched, error) {
	if c.installID == "" || c.organization == "" {
		return Fetched{}, ErrNotEnrolled
	}

	var out ConfigResponse
	if err := c.post(ctx, "/v1/config", req, &out, true); err != nil {
		return Fetched{}, err
	}

	doc, err := config.ParseServedDocument(out.Config)
	if err != nil {
		return Fetched{}, fmt.Errorf("backend: %w", err)
	}

	return Fetched{
		Doc:       doc,
		Raw:       out.Config,
		ExpiresAt: out.ExpiresAt,
	}, nil
}

// post sends a signed JSON request.
func (c *Client) post(ctx context.Context, path string, body, out any, signed bool) (err error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("backend: encode request: %w", err)
	}

	status, raw, err := c.exchange(ctx, path, "", payload, signed)
	if err != nil {
		return err
	}

	switch status {
	case http.StatusOK, http.StatusCreated:
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrUnsupportedVersion, strings.TrimSpace(string(raw)))
	case http.StatusUnauthorized, http.StatusForbidden:
		// Wrapped, not just described: the engine stops the run on this and cannot match on prose.
		return fmt.Errorf("backend: %s refused this install's credentials (HTTP %d): %w",
			path, status, formats.ErrCredentialsRefused)
	default:
		return fmt.Errorf("backend: %s returned HTTP %d: %s", path, status,
			truncate(strings.TrimSpace(string(raw)), 200))
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("backend: decode %s response: %w", path, err)
	}
	return nil
}

// exchange sends one JSON POST and returns its status and bounded body. preamble is the domain
// prefix the signature covers ahead of the body; v1 signs the body alone and passes "".
func (c *Client) exchange(ctx context.Context, path, preamble string, payload []byte, signed bool) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("backend: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Set in the one function every call goes through, so no endpoint can ship without it.
	req.Header.Set(VersionHeader, platform.Current().String())
	req.Header.Set("User-Agent", "trajectory-shipper/"+platform.Current().String())
	if v := platform.OSVersion(); v != "" {
		req.Header.Set(OSHeader, v)
	}
	if b := platform.BootTime(); b != "" {
		req.Header.Set(BootHeader, b)
	}

	if signed {
		if len(c.deviceKey) == 0 || c.organization == "" {
			return 0, nil, ErrNotEnrolled
		}
		// A detached signature over the exact bytes. Scoping uses the install id in the server's
		// authenticated record, never this header, which makes a wider scope unrequestable.
		sig := ed25519.Sign(c.deviceKey, append([]byte(preamble), payload...))
		req.Header.Set("Authorization", fmt.Sprintf("Shipper-Device org=%s, install=%s, sig=%s",
			c.organization, c.installID, EncodeKey(sig)))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("backend: %s: %w", path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("backend: read %s response: %w", path, err)
	}
	return resp.StatusCode, raw, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
