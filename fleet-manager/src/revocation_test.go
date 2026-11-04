package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRevocationSignalIsStableOnEveryDeviceRoute(t *testing.T) {
	server, manager, key, install := enrolledServer(t)
	if err := manager.RevokeInstall(context.Background(), install); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, preamble string }{
		{"/v1/config", ""},
		{"/v2/uploads/authorize", uploadAuthorizePreamble},
		{telemetryPath, telemetryShipperPreamble},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, signedRequest(http.MethodPost, tc.path, []byte(`{}`), install, key, tc.preamble))
			if w.Code != http.StatusForbidden || w.Body.String() != "this install is revoked\n" {
				t.Fatalf("revocation signal = %d %q", w.Code, w.Body.String())
			}
		})
	}
}
