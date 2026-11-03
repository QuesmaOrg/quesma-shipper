package cli

import (
	"context"
	"testing"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
)

func TestRevokedSchedulerWaitsForReplacementEnrollment(t *testing.T) {
	dir := t.TempDir()
	enr := controlplane.Enrollment{InstallID: "original", Organization: "acme", Endpoint: "https://example.com"}
	if err := enr.Save(dir); err != nil {
		t.Fatal(err)
	}
	revoked := &controlplane.Revocation{InstallID: enr.InstallID, Organization: enr.Organization, Endpoint: enr.Endpoint, DetectedAt: time.Now()}
	if err := revoked.Save(dir); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForReenrollment(ctx, dir, revoked, time.Millisecond) }()
	select {
	case err := <-done:
		t.Fatalf("resumed without replacement: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	enr.InstallID = "replacement"
	if err := enr.Save(dir); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("replacement did not resume scheduler")
	}
}
