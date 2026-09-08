//go:build perf

// The isolation the rest of the tier assumes, asserted rather than trusted: every byte figure in
// this package rests on the shipper's only route to the store being the shaped proxy, and a tier
// that stopped seeing half the traffic would not fail, it would just report smaller numbers.
package perf

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// Three claims, in the order they can break: the store has no working address on the host, the
// proxied route does work (so the first claim is isolation and not a dead container), and the
// store cannot reach out either.
func TestTheStoreIsReachableOnlyThroughTheProxy(t *testing.T) {
	ctx := context.Background()

	// The assertion is the dial, not the lookup: where an engine publishes an address anyway,
	// that address must not carry traffic.
	port, err := minioCtr.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Logf("the store published no host port for 9000/tcp: %v", err)
	} else {
		host, err := minioCtr.Host(ctx)
		if err != nil {
			t.Fatalf("the store's host: %v", err)
		}
		addr := net.JoinHostPort(host, port.Port())
		conn, dialErr := net.DialTimeout("tcp", addr, 5*time.Second)
		if dialErr == nil {
			conn.Close()
			t.Errorf("the store answered a direct dial on %s: traffic can reach it without "+
				"passing the proxy, and every byte figure in this package is a lower bound", addr)
		} else {
			t.Logf("the store's published address %s does not carry traffic: %v", addr, dialErr)
		}
	}

	resp, err := http.Get(storeEndpoint + "/minio/health/live")
	if err != nil {
		t.Fatalf("the proxied route to the store is down: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the proxied route to the store answered HTTP %d", resp.StatusCode)
	}

	// An address, not a name: the claim is about routing, not about docker's resolver.
	code, _, err := minioCtr.Exec(ctx, []string{"curl", "-sS", "--max-time", "5", "http://1.1.1.1/"})
	if err != nil {
		t.Fatalf("probe the store's egress: %v", err)
	}
	if code == 0 {
		t.Errorf("the store reached 1.1.1.1 from its own network: the network is not internal, " +
			"and nothing here can claim the shipper's traffic is the only traffic")
	}
}

// The client must not have a route the harness did not give it: the failure worth a test is a sync
// that quietly finds somewhere else to put the data, or reports success having put it nowhere. The
// dead address is configured where a deployment configures one, as a second org's store endpoint.
// The exit code is logged and not asserted: a plain sync reports per-file failures in its summary
// and still exits 0 by design.
func TestASyncAgainstABogusEndpointFailsWithoutFallingBack(t *testing.T) {
	bucket, err := createVersionedBucket(context.Background(),
		fmt.Sprintf("perf-nowhere-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("create the second org's bucket: %v", err)
	}
	// Real, so the emptiness assertion below is an answer rather than a NoSuchBucket.
	const slug = "nowhere"
	w := stageWorldIn(t, slug, deadEndpoint, bucket)
	files := smallCorpusFiles
	stageCorpusFiles(t, w, files)

	before := proxiedBytes(t)
	obs := w.observedSync(t)
	counts := summary(t, obs.Output)
	t.Logf("a sync whose tickets name %s ended %s: shipped %d, failed %d",
		deadEndpoint, obs.exitStatus(), counts["shipped"], counts["failed"])

	if counts["shipped"] != 0 {
		t.Errorf("a sync whose tickets name %s reported %d files shipped:\n%s",
			deadEndpoint, counts["shipped"], obs.Output)
	}
	if counts["failed"] < files {
		t.Errorf("a sync whose tickets name %s reported %d files failed, want at least the %d it "+
			"staged: a file that was neither shipped nor failed went somewhere unaccounted for\n%s",
			deadEndpoint, counts["failed"], files, obs.Output)
	}
	if moved := proxiedBytes(t) - before; moved != 0 {
		t.Errorf("a sync whose tickets name %s moved %d bytes through the real store's proxy",
			deadEndpoint, moved)
	}
	if keys := w.currentKeys(t); len(keys) != 0 {
		t.Errorf("a sync whose tickets name %s landed %d objects in %s", deadEndpoint, len(keys), bucket)
	}
}

// A privileged port, not an ephemeral one bound and closed to learn its number: that number is
// free for anything else in this process to take, and a sync reaching it would look like a bug.
const deadEndpoint = "http://127.0.0.1:1"
