package controlplane_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
)

// telemetrySigningPrefix is the domain-separating preamble, spelled out here rather than taken from
// the constant it tests, so a silent change to the client fails this rather than redefining it.
const telemetrySigningPrefix = "trajectory-shipper-telemetry-v1\nPOST\n/v1/telemetry\n"

// batchID is any valid uuid: what it is does not matter, only that one value is used throughout.
const batchID = "1fe3a22f-e2a1-4e83-bdaf-61dfd9d1bf30"

type submission struct {
	path          string
	authorization string
	body          []byte
	contentType   string
}

// collector stands in for the control plane: it records what arrived and answers with a status.
func collector(t *testing.T, status int, answer string) (*httptest.Server, *submission) {
	t.Helper()
	got := &submission{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.path, got.authorization, got.body = r.URL.Path, r.Header.Get("Authorization"), body
		got.contentType = r.Header.Get("Content-Type")
		w.WriteHeader(status)
		if answer != "" {
			_, _ = io.WriteString(w, answer)
		}
	}))
	t.Cleanup(server.Close)
	return server, got
}

func client(t *testing.T, endpoint string) (*controlplane.Client, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := controlplane.New(controlplane.Options{
		Endpoint:     endpoint,
		InstallID:    fixtureInstallID,
		Organization: "acme",
		DeviceKey:    priv,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, pub
}

func payload(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"event": "install_health", "hostname": "ci-runner-3"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The signature the far end verifies covers the domain prefix and the exact bytes sent, with no
// canonicalization. If this drifts, every submission is refused and nothing here would say why.
func TestTelemetrySignatureCoversThePrefixAndTheExactBody(t *testing.T) {
	server, got := collector(t, http.StatusNoContent, "")
	c, pub := client(t, server.URL)

	if err := c.SubmitTelemetry(context.Background(), "/v1/telemetry",
		batchID, time.Now().UTC(), payload(t)); err != nil {
		t.Fatalf("submit: %v", err)
	}

	want := "Shipper-Device org=acme, install=" + fixtureInstallID + ", sig="
	if !strings.HasPrefix(got.authorization, want) {
		t.Fatalf("authorization %q does not open with %q", got.authorization, want)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got.authorization, want))
	if err != nil {
		t.Fatalf("sig is not base64: %v", err)
	}
	if !ed25519.Verify(pub, append([]byte(telemetrySigningPrefix), got.body...), sig) {
		t.Fatal("the signature does not verify over the prefix and the body that was sent")
	}
	// A body-only signature is what the v1 routes use; accepting it here would mean the prefix is
	// not actually in the signed bytes.
	if ed25519.Verify(pub, got.body, sig) {
		t.Fatal("the signature verifies without the prefix, so the domain separation is absent")
	}
	if got.contentType != "application/json" {
		t.Errorf("content type %q", got.contentType)
	}
}

// The envelope is exactly four fields. The control plane strict-decodes it, so a fifth would be
// refused for every install at once.
func TestTelemetryEnvelopeIsExactlyTheContract(t *testing.T) {
	server, got := collector(t, http.StatusNoContent, "")
	c, _ := client(t, server.URL)

	issued := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	if err := c.SubmitTelemetry(context.Background(), "/v1/telemetry",
		batchID, issued, payload(t)); err != nil {
		t.Fatal(err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"schema", "batch_id", "issued_at", "payload"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("the envelope has no %s", name)
		}
	}
	if len(fields) != 4 {
		t.Errorf("the envelope carries %d fields, the contract allows 4: %v", len(fields), slices.Sorted(maps.Keys(fields)))
	}
	if string(fields["issued_at"]) != `"2026-09-18T12:00:00Z"` {
		t.Errorf("issued_at = %s, want the caller's stamp in RFC3339", fields["issued_at"])
	}
}

// The path is where a submission goes and what the signature covers, and it comes from served
// configuration. It is used as given.
func TestTelemetryUsesTheServedPath(t *testing.T) {
	server, got := collector(t, http.StatusNoContent, "")
	c, _ := client(t, server.URL)

	if err := c.SubmitTelemetry(context.Background(), "/v1/telemetry",
		batchID, time.Now().UTC(), payload(t)); err != nil {
		t.Fatal(err)
	}
	if got.path != "/v1/telemetry" {
		t.Errorf("posted to %q", got.path)
	}
}

// Each status has one meaning, and the caller acts on which error came back rather than on a code.
func TestTelemetryStatusMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		want   error
	}{
		"accepted":       {http.StatusNoContent, nil},
		"also accepted":  {http.StatusOK, nil},
		"disabled":       {http.StatusForbidden, controlplane.ErrTelemetryDisabled},
		"invalid":        {http.StatusBadRequest, controlplane.ErrTelemetryRejected},
		"too large":      {http.StatusRequestEntityTooLarge, controlplane.ErrTelemetryRejected},
		"unprocessable":  {http.StatusUnprocessableEntity, controlplane.ErrTelemetryRejected},
		"conflict":       {http.StatusConflict, controlplane.ErrTelemetryRejected},
		"rate limited":   {http.StatusTooManyRequests, controlplane.ErrTelemetryUnavailable},
		"collector down": {http.StatusBadGateway, controlplane.ErrTelemetryUnavailable},
		"upstream slow":  {http.StatusGatewayTimeout, controlplane.ErrTelemetryUnavailable},
		"unprovisioned":  {http.StatusServiceUnavailable, controlplane.ErrTelemetryUnavailable},
	} {
		server, _ := collector(t, tc.status, `{"error":"telemetry_something"}`)
		c, _ := client(t, server.URL)
		err := c.SubmitTelemetry(context.Background(), "/v1/telemetry",
			batchID, time.Now().UTC(), payload(t))
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: got %v, want %v", name, err, tc.want)
		}
	}
}

// An empty path is a disabled organization, and asking anyway would spend a signed request to be
// told what configuration already said.
func TestTelemetryWithNoPathIsDisabledWithoutARequest(t *testing.T) {
	server, got := collector(t, http.StatusNoContent, "")
	c, _ := client(t, server.URL)

	err := c.SubmitTelemetry(context.Background(), "", batchID,
		time.Now().UTC(), payload(t))
	if !errors.Is(err, controlplane.ErrTelemetryDisabled) {
		t.Fatalf("got %v, want controlplane.ErrTelemetryDisabled", err)
	}
	if got.path != "" {
		t.Error("a request was sent for a disabled organization")
	}
}

// Refused here rather than spending a request to be refused there.
func TestAnOversizedSubmissionIsRefusedLocally(t *testing.T) {
	server, got := collector(t, http.StatusNoContent, "")
	c, _ := client(t, server.URL)

	big, err := json.Marshal(map[string]string{"event": strings.Repeat("x", controlplane.MaxTelemetryBody)})
	if err != nil {
		t.Fatal(err)
	}
	err = c.SubmitTelemetry(context.Background(), "/v1/telemetry",
		batchID, time.Now().UTC(), big)
	if !errors.Is(err, controlplane.ErrTelemetryRejected) {
		t.Fatalf("got %v, want controlplane.ErrTelemetryRejected", err)
	}
	if got.path != "" {
		t.Error("an oversized submission was sent anyway")
	}
}

// The route is fixed by the protocol: the control plane builds the same preamble from the same
// literal, so a served path that differs would be signed here and verified against something else.
// Refused locally, where the reason is visible, rather than as an unexplained 401.
func TestAServedPathThatIsNotTheProtocolsIsRefused(t *testing.T) {
	server, got := collector(t, http.StatusNoContent, "")
	c, _ := client(t, server.URL)

	err := c.SubmitTelemetry(context.Background(), "/v1/elsewhere", batchID, time.Now().UTC(), payload(t))
	if !errors.Is(err, controlplane.ErrTelemetryRejected) {
		t.Fatalf("got %v, want controlplane.ErrTelemetryRejected", err)
	}
	if got.path != "" {
		t.Error("a submission was signed for one path and sent to another")
	}
}
