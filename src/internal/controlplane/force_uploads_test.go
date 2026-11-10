package controlplane_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	protocol "github.com/QuesmaOrg/quesma-shipper/shipper-protocol"
)

func TestForceUploadsUsesV3AndNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"old server", http.StatusNotFound, "not found", "upgrade Fleet Manager"},
		{"old method", http.StatusMethodNotAllowed, "not allowed", "upgrade Fleet Manager"},
		{"wrong answer", http.StatusOK, `{"tickets":[` + alreadyPresentTicketJSON() + `]}`, "already_present"},
		{"PUT ticket", http.StatusOK, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pub, key, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v3/uploads/authorize" {
					t.Errorf("wrong endpoint: %s", r.URL.Path)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if !strings.Contains(string(body), `"force_resend":true`) {
					t.Error("request did not explicitly request replacement")
				}
				_, sig, _ := strings.Cut(r.Header.Get("Authorization"), "sig=")
				signature, err := base64.StdEncoding.DecodeString(sig)
				if err != nil {
					t.Error(err)
					return
				}
				prefix := "trajectory-shipper-upload-authorize-v3\nPOST\n/v3/uploads/authorize\n"
				if !ed25519.Verify(pub, append([]byte(prefix), body...), signature) {
					t.Error("signature does not bind v3")
				}
				response := []byte(tc.body)
				if tc.want == "" {
					response, err = fs.ReadFile(protocol.FS, "fixtures/v3/uploads-authorize/response.json")
					if err != nil {
						t.Error(err)
						return
					}
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write(response)
			}))
			defer srv.Close()
			client, err := controlplane.New(controlplane.Options{Endpoint: srv.URL, InstallID: fixtureInstallID, Organization: "acme", DeviceKey: key})
			if err != nil {
				t.Fatal(err)
			}
			req := sampleAuthorizeRequest()
			req.ForceResend = true
			_, err = client.AuthorizeUploads(context.Background(), req)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("result: %v, want %q", err, tc.want)
			}
			if calls != 1 {
				t.Fatalf("made %d calls; force must not fall back to v2", calls)
			}
		})
	}
}
