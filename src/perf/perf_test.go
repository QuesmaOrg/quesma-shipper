//go:build perf

// Package perf measures how a sync behaves when the store is far away: Toxiproxy shapes the link to
// a MinIO that sits alone on an internal network, so every shipper byte is on a counter while the
// harness's own admin traffic goes over a second, never-shaped proxy. Bounds are ratios or multiples
// of the injected RTT, never absolute times, and the toxic shapes delay only, not bandwidth or loss.
package perf

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	toxiproxy "github.com/Shopify/toxiproxy/v2/client"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Pinned: a floating MinIO changes what the store does under concurrency and a floating toxiproxy
// renames the metrics this file parses, either without a line of shipper code changing.
const (
	minioImage     = "minio/minio:RELEASE.2025-04-22T22-12-26Z"
	toxiproxyImage = "ghcr.io/shopify/toxiproxy:2.12.0"
)

const (
	// How toxiproxy reaches MinIO; there is no host-mapped alternative, which is the point.
	minioAlias = "minio"

	// The shipper's listener and the harness's: any ports but 8474, which is the API.
	proxyListenPort  = "8666"
	adminListenPort  = "8667"
	toxiproxyAPIPort = "8474"

	// Separate label sets in the metrics, so the byte counters answer for the shipper alone.
	storeProxyName = "minio"
	adminProxyName = "minio-admin"
)

var (
	minioAccess string
	minioSecret string

	// Kept for the isolation assertions, which ask the container what it published.
	minioCtr testcontainers.Container

	// Lists over the unshaped admin proxy, so its traffic never lands on the shipper's counters.
	adminS3 *awss3.Client

	// The harness's route and the shipper's: the origin every ticket is presigned against.
	adminEndpoint string
	storeEndpoint string

	// The one bucket this tier writes to; worlds are separated by install root, not by bucket.
	perfBucket string

	toxi       *toxiproxy.Client
	storeProxy *toxiproxy.Proxy

	// The proxy's control API, its exposition page, and MinIO's own.
	toxiAPIURL      string
	metricsURL      string
	minioMetricsURL string
)

func TestMain(m *testing.M) {
	// Before any container: this binary is what probeMemoryCap re-executes as the hog.
	if spec, ok := os.LookupEnv(memoryHogEnv); ok {
		os.Exit(runMemoryHog(spec))
	}
	os.Exit(run(m))
}

// Exists so the teardown can be a defer: TestMain cannot have one, since os.Exit does not unwind.
func run(m *testing.M) int {
	ctx := context.Background()
	// Package-level temp dirs, so there is no t.TempDir to clean them up.
	defer removeCorpusMaster()
	defer removeBinaries()

	// Two networks, not one: internalNet has no NAT and no default route, and toxiproxy is the
	// only container on both.
	edgeNet, err := network.New(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: create the edge network: %v\n", err)
		return 1
	}
	defer func() {
		if err := edgeNet.Remove(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "perf: remove the edge network: %v\n", err)
		}
	}()
	internalNet, err := network.New(ctx, network.WithInternal())
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: create the internal network: %v\n", err)
		return 1
	}
	defer func() {
		if err := internalNet.Remove(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "perf: remove the internal network: %v\n", err)
		}
	}()

	minio, err := tcminio.Run(ctx, minioImage,
		network.WithNetwork([]string{minioAlias}, internalNet),
		// Public scrape auth because the S3 request count is read off a metrics page, which
		// otherwise answers 403. CI_CD drops MinIO's preallocation, keeping the store out of the
		// way of the memory budgets the scenarios next door run under.
		testcontainers.WithEnv(map[string]string{
			"MINIO_PROMETHEUS_AUTH_TYPE": "public",
			"CI_CD":                      "true",
		}),
		// Docker publishes nothing for an internal-only container and the pinned ARM64 image's
		// curl is not executable, so wait on MinIO's own ready log instead.
		testcontainers.WithWaitStrategy(
			wait.ForLog("API:").
				WithStartupTimeout(2*time.Minute)),
	)
	defer func() {
		if err := testcontainers.TerminateContainer(minio); err != nil {
			fmt.Fprintf(os.Stderr, "perf: terminate MinIO: %v\n", err)
		}
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: start MinIO: %v\n", err)
		return 1
	}
	minioCtr = minio
	minioAccess, minioSecret = minio.Username, minio.Password

	// Plain testcontainers.Run rather than modules/toxiproxy: that module's own -config cannot be
	// combined with the -proxy-metrics these byte counters come from.
	proxyCtr, err := testcontainers.Run(ctx, toxiproxyImage,
		// The ordinary network first: testcontainers attaches the first at create time, and a
		// container created on an internal network gets no port bindings at all (moby #36174).
		network.WithNetwork([]string{"toxiproxy"}, edgeNet),
		network.WithNetwork([]string{"toxiproxy"}, internalNet),
		testcontainers.WithExposedPorts(
			toxiproxyAPIPort+"/tcp", proxyListenPort+"/tcp", adminListenPort+"/tcp"),
		// The image defaults the API to localhost, which from outside the container is nothing.
		testcontainers.WithCmd("-host=0.0.0.0", "-proxy-metrics"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/version").WithPort(toxiproxyAPIPort+"/tcp")),
	)
	defer func() {
		if err := testcontainers.TerminateContainer(proxyCtr); err != nil {
			fmt.Fprintf(os.Stderr, "perf: terminate toxiproxy: %v\n", err)
		}
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: start toxiproxy: %v\n", err)
		return 1
	}

	apiAddr, err := hostAddr(ctx, proxyCtr, toxiproxyAPIPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: toxiproxy API address: %v\n", err)
		return 1
	}
	listenAddr, err := hostAddr(ctx, proxyCtr, proxyListenPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: toxiproxy listener address: %v\n", err)
		return 1
	}
	adminAddr, err := hostAddr(ctx, proxyCtr, adminListenPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: toxiproxy admin listener address: %v\n", err)
		return 1
	}
	toxiAPIURL = "http://" + apiAddr
	metricsURL = toxiAPIURL + "/metrics"
	storeEndpoint = "http://" + listenAddr
	adminEndpoint = "http://" + adminAddr
	// The v3 page, not the v2 cluster one: v2 serves a ten-second cached snapshot, so a count read
	// either side of a sub-second run would read the same number twice.
	minioMetricsURL = adminEndpoint + "/minio/metrics/v3/api/requests"

	toxi = toxiproxy.NewClient(apiAddr)
	// 0.0.0.0 so the mapped port reaches it; the docker alias upstream, resolved on the internal net.
	storeProxy, err = toxi.CreateProxy(storeProxyName, "0.0.0.0:"+proxyListenPort, minioAlias+":9000")
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: create the store proxy: %v\n", err)
		return 1
	}
	// The harness's own route: never shaped, and its bytes carry a different label, so listing keys
	// cannot show up in what the shipper is charged.
	if _, err := toxi.CreateProxy(adminProxyName, "0.0.0.0:"+adminListenPort, minioAlias+":9000"); err != nil {
		fmt.Fprintf(os.Stderr, "perf: create the admin proxy: %v\n", err)
		return 1
	}

	adminS3 = awss3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider(minioAccess, minioSecret, ""),
	}, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(adminEndpoint)
		o.UsePathStyle = true
	})

	// Minted before the protocol peer starts, because it signs the tickets used by the measured path.
	if err := mintIngestUser(ctx, minio); err != nil {
		fmt.Fprintf(os.Stderr, "perf: mint the write-only ingestion user: %v\n", err)
		return 1
	}
	perfBucket, err = createVersionedBucket(ctx, fmt.Sprintf("perf-%d", time.Now().UnixNano()))
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: create the bucket: %v\n", err)
		return 1
	}
	startProtocolPeer()
	defer stopProtocolPeer()

	code := m.Run()
	if code != 0 {
		dumpProxyState()
	}
	return code
}

// Versioning matters here: mirror objects are overwritten in place, so "the steady-state run added
// no versions" can only be asked of a versioned bucket.
func createVersionedBucket(ctx context.Context, name string) (string, error) {
	if _, err := adminS3.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		return "", err
	}
	if _, err := adminS3.PutBucketVersioning(ctx, &awss3.PutBucketVersioningInput{
		Bucket: aws.String(name),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		return "", err
	}
	return name, nil
}

// Whether a timing scenario failed on slower code or on a toxic that was never attached is the first
// question, and the API that answers it dies with the container. Failure path only.
func dumpProxyState() {
	path := filepath.Join(resultsDir(), "toxiproxy-state.json")
	resp, err := http.Get(toxiAPIURL + "/proxies")
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: read the proxy state for %s: %v\n", path, err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf: read the proxy state for %s: %v\n", path, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "perf: create the directory for %s: %v\n", path, err)
		return
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "perf: write %s: %v\n", path, err)
		return
	}
	fmt.Fprintf(os.Stderr, "perf: the proxies and their toxics as they were at the end: %s\n", path)
}

func hostAddr(ctx context.Context, c testcontainers.Container, port string) (string, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}
	mapped, err := c.MappedPort(ctx, port+"/tcp")
	if err != nil {
		return "", err
	}
	return host + ":" + mapped.Port(), nil
}

// --- link shaping ------------------------------------------------------------

// Half per direction, because the toxic delays one stream and a request-response pair crosses both.
func withLatency(t *testing.T, rtt, jitter time.Duration) {
	t.Helper()
	half := rtt / 2
	for _, stream := range []string{"upstream", "downstream"} {
		_, err := storeProxy.AddToxic("latency_"+stream, "latency", stream, 1,
			toxiproxy.Attributes{
				"latency": half.Milliseconds(),
				"jitter":  (jitter / 2).Milliseconds(),
			})
		if err != nil {
			t.Fatalf("add %s latency toxic: %v", stream, err)
		}
	}
	// /reset removes every toxic and re-enables every proxy, however the test exits.
	t.Cleanup(func() {
		if err := toxi.ResetState(); err != nil {
			t.Errorf("reset toxiproxy: %v", err)
		}
	})
	assertShapingIsLive(t, rtt)
}

// Proves the toxic is on the wire before a test spends minutes measuring under it. A full request,
// not a dial: the toxic delays data, so a connect-only probe would pass with no shaping at all.
func assertShapingIsLive(t *testing.T, rtt time.Duration) {
	t.Helper()
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   30 * time.Second,
	}
	start := time.Now()
	resp, err := client.Get(storeEndpoint + "/minio/health/live")
	if err != nil {
		t.Fatalf("probe the shaped link: %v", err)
	}
	resp.Body.Close()
	elapsed := time.Since(start)

	// Three quarters of the injected rtt: slack for the handshake and the scheduler, not for a
	// missing toxic.
	if floor := rtt * 3 / 4; elapsed < floor {
		t.Fatalf("a round trip through the proxy took %v with %v of latency configured; "+
			"the toxic is not on the wire and every timing below would be meaningless",
			elapsed, rtt)
	}
	t.Logf("shaping live: %v round trip at %v configured rtt", elapsed.Round(time.Millisecond), rtt)
}

// --- byte counters -----------------------------------------------------------

// What one run cost the store: bytes each way through the shaped proxy, and S3 calls served.
type storeCounters struct {
	up, down, requests int64
}

// The difference between the counters read either side of fn, since they run for the life of the
// tier. Nothing else may talk to the store while fn runs: adminS3 lands in the same request count.
func aroundStore(t *testing.T, fn func()) storeCounters {
	t.Helper()
	upBefore, downBefore := proxiedBytesByDirection(t)
	requestsBefore := s3RequestCount(t)

	fn()

	up, down := proxiedBytesByDirection(t)
	return storeCounters{
		up:       up - upBefore,
		down:     down - downBefore,
		requests: s3RequestCount(t) - requestsBefore,
	}
}

// One reading of the shaped proxy's four byte counters; toxiproxy counts each link's bytes twice,
// once received and once sent, so total is not up+down.
type storeBytes struct {
	receivedUp, receivedDown int64
	sentUp, sentDown         int64
}

func (b storeBytes) total() int64 {
	return b.receivedUp + b.receivedDown + b.sentUp + b.sentDown
}

// Call only after the child has exited: toxiproxy accounts a link's bytes when the link closes, and
// even then the flush trails the process, hence the poll for a value that stopped moving.
func proxiedBytes(t *testing.T) int64 {
	t.Helper()
	return settledStoreBytes(t).total()
}

// Only the received counters, so the figures are bytes and not bytes counted twice.
func proxiedBytesByDirection(t *testing.T) (up, down int64) {
	t.Helper()
	b := settledStoreBytes(t)
	return b.receivedUp, b.receivedDown
}

func settledStoreBytes(t *testing.T) storeBytes {
	t.Helper()
	const (
		stableReads = 3
		interval    = 250 * time.Millisecond
		deadline    = 20 * time.Second
	)
	give := time.Now().Add(deadline)
	last, same := int64(-1), 0
	for {
		b := scrapeStoreBytes(t)
		v := b.total()
		if v == last {
			if same++; same >= stableReads-1 {
				return b
			}
		} else {
			last, same = v, 0
		}
		if time.Now().After(give) {
			t.Fatalf("toxiproxy's byte counters never settled: last read %d bytes", v)
		}
		time.Sleep(interval)
	}
}

// Filtered to the shaped proxy by label: a sum across both would charge the shipper for every
// assertion the harness makes about it.
func scrapeStoreBytes(t *testing.T) storeBytes {
	t.Helper()
	var b storeBytes
	eachSample(t, metricsURL, scrapePage(t, metricsURL), func(name, labels string, value float64) {
		if name != "toxiproxy_proxy_received_bytes_total" &&
			name != "toxiproxy_proxy_sent_bytes_total" {
			return
		}
		if !strings.Contains(labels, `proxy="`+storeProxyName+`"`) {
			return
		}
		received := name == "toxiproxy_proxy_received_bytes_total"
		switch {
		case strings.Contains(labels, `direction="upstream"`) && received:
			b.receivedUp += int64(value)
		case strings.Contains(labels, `direction="upstream"`):
			b.sentUp += int64(value)
		case strings.Contains(labels, `direction="downstream"`) && received:
			b.receivedDown += int64(value)
		default:
			b.sentDown += int64(value)
		}
	})
	return b
}

// Named here because a pinned MinIO that renamed it would report every run as zero requests.
const s3RequestCountMetric = "minio_api_requests_total"

// A running total like the byte counters, read either side of a child run. Unlike them it counts the
// harness too, so nothing else may run between the two readings.
func s3RequestCount(t *testing.T) int64 {
	t.Helper()
	page := scrapePage(t, minioMetricsURL)
	if !strings.Contains(page, s3RequestCountMetric) {
		t.Fatalf("no %s on %s: the pinned MinIO renamed the counter and every request "+
			"figure would read zero", s3RequestCountMetric, minioMetricsURL)
	}
	var total int64
	eachSample(t, minioMetricsURL, page, func(name, labels string, value float64) {
		if name == s3RequestCountMetric && strings.Contains(labels, `type="s3"`) {
			total += int64(value)
		}
	})
	return total
}

// Whole, into memory: the text lets a caller catch a renamed metric instead of summing to zero.
func scrapePage(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("scrape %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return string(body)
}

// Parsed by hand rather than with expfmt, which would be this module's only Prometheus dependency.
func eachSample(t *testing.T, url, page string, fn func(name, labels string, value float64)) {
	t.Helper()
	for line := range strings.SplitSeq(page, "\n") {
		name, labelled, ok := strings.Cut(line, "{")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		labels, value, ok := strings.Cut(labelled, "} ")
		if !ok {
			t.Fatalf("unparseable sample line from %s: %q", url, line)
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			t.Fatalf("unparseable sample value in %q: %v", line, err)
		}
		fn(name, labels, f)
	}
}
