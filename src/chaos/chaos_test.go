//go:build chaos

// Package chaos reproduces field network faults against the real shipper binary in a Linux container,
// resolving through a controllable DNS responder (testdata/dnsd) and reaching the in-process far ends
// (peer_test.go) only through toxiproxy, TLS end to end. The tests pin what the shipper records today.
package chaos

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	toxiproxy "github.com/Shopify/toxiproxy/v2/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Pinned, and pre-pulled by CI.
const (
	toxiproxyImage = "ghcr.io/shopify/toxiproxy:2.12.0"
	alpineImage    = "alpine:3.20.3"
)

const (
	zone             = "chaos.test"
	toxiproxyAPIPort = "8474"
	dnsAPIPort       = "8053"
)

var (
	buildDir      string
	shipperPath   string
	dnsdPath      string
	caPath        string
	chaosNet      *testcontainers.DockerNetwork
	toxi          *toxiproxy.Client
	dnsAPI        string
	dnsIP         string
	blackholeIP   string
	cpUpstream    string
	storeUpstream string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// Exists so the teardown can be deferred: TestMain cannot defer, since os.Exit does not unwind.
func run(m *testing.M) int {
	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()
	if err := setup(context.Background(), &cleanups); err != nil {
		fmt.Fprintf(os.Stderr, "chaos: %v\n", err)
		return 1
	}
	return m.Run()
}

func setup(ctx context.Context, cleanups *[]func()) error {
	dir, err := os.MkdirTemp("", "shipper-chaos")
	if err != nil {
		return err
	}
	buildDir = dir
	*cleanups = append(*cleanups, func() { _ = os.RemoveAll(dir) })
	if err := buildBinaries(); err != nil {
		return err
	}
	leaf, err := writeTestCA()
	if err != nil {
		return err
	}

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{leaf}}
	cp := httptest.NewUnstartedServer(http.HandlerFunc(far.serveControlPlane))
	cp.TLS = tlsConfig
	cp.StartTLS()
	*cleanups = append(*cleanups, cp.Close)
	store := httptest.NewUnstartedServer(http.HandlerFunc(far.serveStore))
	store.TLS = tlsConfig
	store.StartTLS()
	*cleanups = append(*cleanups, store.Close)
	cpPort, storePort := listenerPort(cp), listenerPort(store)
	cpUpstream = testcontainers.HostInternal + ":" + strconv.Itoa(cpPort)
	storeUpstream = testcontainers.HostInternal + ":" + strconv.Itoa(storePort)

	// Not internal: Docker's isolation between this network and the void one is the blackhole.
	chaosNet, err = network.New(ctx)
	if err != nil {
		return fmt.Errorf("create the chaos network: %w", err)
	}
	*cleanups = append(*cleanups, func() { _ = chaosNet.Remove(ctx) })
	voidNet, err := network.New(ctx)
	if err != nil {
		return fmt.Errorf("create the void network: %w", err)
	}
	*cleanups = append(*cleanups, func() { _ = voidNet.Remove(ctx) })

	proxy, err := testcontainers.Run(ctx, toxiproxyImage,
		network.WithNetwork([]string{"toxiproxy"}, chaosNet),
		testcontainers.WithExposedPorts(toxiproxyAPIPort+"/tcp"),
		testcontainers.WithCmd("-host=0.0.0.0"),
		testcontainers.WithHostPortAccess(cpPort, storePort),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/version").WithPort(toxiproxyAPIPort+"/tcp")),
	)
	*cleanups = append(*cleanups, func() { _ = testcontainers.TerminateContainer(proxy) })
	if err != nil {
		return fmt.Errorf("start toxiproxy: %w", err)
	}
	apiAddr, err := proxy.PortEndpoint(ctx, toxiproxyAPIPort+"/tcp", "")
	if err != nil {
		return err
	}
	toxi = toxiproxy.NewClient(apiAddr)
	proxyIP, err := ipOn(ctx, proxy, chaosNet)
	if err != nil {
		return err
	}

	// A peer that exists only on the void network: SYNs to it from the chaos network are dropped.
	void, err := testcontainers.Run(ctx, alpineImage,
		network.WithNetwork([]string{"void"}, voidNet),
		testcontainers.WithCmd("sleep", "infinity"),
	)
	*cleanups = append(*cleanups, func() { _ = testcontainers.TerminateContainer(void) })
	if err != nil {
		return fmt.Errorf("start the void peer: %w", err)
	}
	if blackholeIP, err = ipOn(ctx, void, voidNet); err != nil {
		return err
	}

	dns, err := testcontainers.Run(ctx, alpineImage,
		network.WithNetwork([]string{"dns"}, chaosNet),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			HostFilePath: dnsdPath, ContainerFilePath: "/usr/local/bin/chaos-dnsd", FileMode: 0o755,
		}),
		testcontainers.WithCmd("/usr/local/bin/chaos-dnsd", "-zone", zone, "-answer", proxyIP),
		testcontainers.WithExposedPorts(dnsAPIPort+"/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/healthz").WithPort(dnsAPIPort+"/tcp")),
	)
	*cleanups = append(*cleanups, func() { _ = testcontainers.TerminateContainer(dns) })
	if err != nil {
		return fmt.Errorf("start the DNS server: %w", err)
	}
	if dnsAPI, err = dns.PortEndpoint(ctx, dnsAPIPort+"/tcp", "http"); err != nil {
		return err
	}
	if dnsIP, err = ipOn(ctx, dns, chaosNet); err != nil {
		return err
	}
	return nil
}

// Both binaries for the Docker host's architecture, CGO off as released, so the resolver is Go's own.
func buildBinaries() error {
	build := func(out, pkg string) error {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = ".."
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %v: %s", pkg, err, b)
		}
		return nil
	}
	shipperPath = filepath.Join(buildDir, "quesma-shipper")
	dnsdPath = filepath.Join(buildDir, "chaos-dnsd")
	if err := build(shipperPath, "./cmd/quesma-shipper"); err != nil {
		return err
	}
	return build(dnsdPath, "./chaos/testdata/dnsd")
}

// One CA and one wildcard leaf for every name in the zone; the shipper trusts the CA via SSL_CERT_FILE.
func writeTestCA() (tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "chaos test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "*." + zone},
		DNSNames:  []string{"*." + zone},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	caPath = filepath.Join(buildDir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}, nil
}

func listenerPort(s *httptest.Server) int { return s.Listener.Addr().(*net.TCPAddr).Port }

func ipOn(ctx context.Context, c testcontainers.Container, nw *testcontainers.DockerNetwork) (string, error) {
	inspect, err := c.Inspect(ctx)
	if err != nil {
		return "", err
	}
	settings, ok := inspect.NetworkSettings.Networks[nw.Name]
	if !ok || !settings.IPAddress.IsValid() {
		return "", fmt.Errorf("container %s has no address on network %s", c.GetContainerID(), nw.Name)
	}
	return settings.IPAddress.String(), nil
}
