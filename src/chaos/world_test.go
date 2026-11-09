//go:build chaos

// One scenario's machine: a container holding the shipper, enrolled against the in-process control
// plane through its own pair of proxies and its own host names, so a fault on one never reaches another.
// Every tick is one `run --once`; the state directory stays in the container between ticks.
package chaos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	toxiproxy "github.com/Shopify/toxiproxy/v2/client"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/network"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
)

// Fixed and published: it protects nothing and must never be used elsewhere.
const (
	testAgeIdentity = "AGE-SECRET-KEY-1JF0Y36Z2RMJNJNN2AYUUF6HMHZVK3FCGK4GUADGRF9M3R57S2UCSDMJWD7"
	testNameKey     = "0101010101010101010101010101010101010101010101010101010101010101"
)

const (
	home        = "/home/tester"
	stateDir    = home + "/.local/state/trajectory-shipper"
	configDir   = home + "/.config/trajectory-shipper"
	projectsDir = home + "/.claude/projects/-home-tester-work-demo"

	// A deadlock catcher well past the slowest scenario, not a bound on any of them.
	tickDeadline = 6 * time.Minute

	// Same role for a harness exec or copy, which takes seconds.
	harnessDeadline = time.Minute

	// One core, so authorization groups form the same way on every machine: four objects each.
	childGOMAXPROCS = "1"
)

var nextPort atomic.Int32

func init() { nextPort.Store(21000) }

type toxiAttrs = toxiproxy.Attributes

type stage struct {
	t        *testing.T
	cpHost   string
	store    string
	cp       *toxiproxy.Proxy
	storeTox *toxiproxy.Proxy
	in       *install
	ctr      testcontainers.Container
	staged   int
	ticks    []tickResult

	// Mirror objects one staged file becomes, and what the fault tick left behind.
	perFile      int
	afterFault   int
	faultMessage string

	// Set by a scenario whose fault runs on its own goroutine; joins it once the fault tick ends.
	afterFaultTick func() error
}

type tickResult struct {
	label   string
	exit    int
	output  string
	elapsed time.Duration
	started time.Time
}

func newStage(t *testing.T, slug string) *stage {
	t.Helper()
	ctx := context.Background()
	s := &stage{t: t, cpHost: "cp-" + slug + "." + zone, store: "store-" + slug + "." + zone}
	last := nextPort.Add(2)
	cpPort, storePort := last-1, last

	var err error
	if s.cp, err = toxi.CreateProxy("cp-"+slug, fmt.Sprintf("0.0.0.0:%d", cpPort), cpUpstream); err != nil {
		t.Fatalf("create the control-plane proxy: %v", err)
	}
	if s.storeTox, err = toxi.CreateProxy("store-"+slug, fmt.Sprintf("0.0.0.0:%d", storePort), storeUpstream); err != nil {
		t.Fatalf("create the store proxy: %v", err)
	}
	t.Cleanup(func() { _ = s.cp.Delete(); _ = s.storeTox.Delete() })

	installID := uuid.NewString()
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	storeOrigin := fmt.Sprintf("https://%s:%d", s.store, storePort)
	s.in = far.register(installID, pub, storeOrigin)

	s.ctr, err = testcontainers.Run(ctx, alpineImage,
		network.WithNetwork(nil, chaosNet),
		testcontainers.WithFiles(
			testcontainers.ContainerFile{HostFilePath: shipperPath, ContainerFilePath: "/usr/local/bin/quesma-shipper", FileMode: 0o755},
			testcontainers.ContainerFile{HostFilePath: caPath, ContainerFilePath: "/etc/chaos/ca.pem", FileMode: 0o644},
		),
		testcontainers.WithEnv(map[string]string{
			"HOME":            home,
			"XDG_CONFIG_HOME": home + "/.config",
			"XDG_STATE_HOME":  home + "/.local/state",
			"SSL_CERT_FILE":   "/etc/chaos/ca.pem",
			"GOMAXPROCS":      childGOMAXPROCS,
		}),
		testcontainers.WithCmd("sleep", "infinity"),
	)
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(s.ctr) })
	if err != nil {
		t.Fatalf("start the shipper container: %v", err)
	}
	// Straight at the chaos DNS: Docker's embedded resolver would retry and rewrite the failures.
	s.sh("printf 'nameserver %s\\n' > /etc/resolv.conf && mkdir -p -m 700 %s %s %s",
		dnsIP, stateDir, configDir, projectsDir)

	id, err := age.ParseX25519Identity(testAgeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	s.writeJSON(stateDir+"/identity.json", map[string]any{
		"identity_schema": 1, "install_id": installID, "age_identity": testAgeIdentity,
		"age_recipient": id.Recipient().String(), "name_key": testNameKey,
		"created_at": "2026-07-01T12:00:00Z",
	})
	s.writeJSON(stateDir+"/enrollment.json", map[string]any{
		"enrollment_schema": 2, "install_id": installID, "organization": chaosOrg,
		"endpoint":    fmt.Sprintf("https://%s:%d", s.cpHost, cpPort),
		"device_key":  base64.StdEncoding.EncodeToString(private),
		"enrolled_at": "2026-07-01T12:00:00Z",
	})
	// Account snapshot off: its quarter-hourly name would make the upload count depend on the clock.
	s.write(configDir+"/config.yaml", []byte("config_version: 1\n"+
		"sources:\n"+
		"  - id: claude-account\n"+
		"    enabled: false\n"+
		"upload_targets:\n"+
		"  - origin: "+storeOrigin+"\n"+
		"    addressing: path-style\n"+
		"    path_prefix: /"+chaosBucket+"\n"))

	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, tr := range s.ticks {
			t.Logf("--- %s tick: exit %d after %s\n%s", tr.label, tr.exit, tr.elapsed.Round(time.Millisecond), tr.output)
		}
		t.Logf("--- last-failure.json\n%s", s.readFile(stateDir+"/last-failure.json"))
	})
	return s
}

func (s *stage) sh(format string, args ...any) {
	s.t.Helper()
	cmd := fmt.Sprintf(format, args...)
	ctx, cancel := context.WithTimeout(context.Background(), harnessDeadline)
	defer cancel()
	code, out, err := s.ctr.Exec(ctx, []string{"sh", "-c", cmd}, tcexec.Multiplexed())
	if err != nil {
		s.t.Fatalf("exec %q: %v", cmd, err)
	}
	raw, _ := io.ReadAll(out)
	if code != 0 {
		s.t.Fatalf("exec %q exited %d: %s", cmd, code, raw)
	}
}

func (s *stage) write(path string, body []byte) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), harnessDeadline)
	defer cancel()
	// 0600: the identity loader refuses anything wider.
	if err := s.ctr.CopyToContainer(ctx, body, path, 0o600); err != nil {
		s.t.Fatalf("copy %s into the container: %v", path, err)
	}
}

func (s *stage) writeJSON(path string, v any) {
	s.t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	s.write(path, raw)
}

func (s *stage) readFile(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), harnessDeadline)
	defer cancel()
	rc, err := s.ctr.CopyFileFromContainer(ctx, path)
	if err != nil {
		return fmt.Sprintf("(unreadable: %v)", err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	return string(raw)
}

// Adds n new session transcripts of about size bytes each, the next indexes after those already staged.
func (s *stage) stageTranscripts(n, size int) {
	s.t.Helper()
	for range n {
		i := s.staged
		s.staged++
		session := fmt.Sprintf("c%07x-a%03x-4b%02x-8c%02x-e%011x", i, i, i%256, i%256, i)
		s.write(projectsDir+"/"+session+".jsonl", transcript(int64(i), session, size))
	}
}

// One tick as the daemon would run it, timed from the test's side of the exec.
func (s *stage) tick(label string) tickResult {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tickDeadline)
	defer cancel()
	started := time.Now()
	code, out, err := s.ctr.Exec(ctx, []string{"/usr/local/bin/quesma-shipper", "run", "--once"}, tcexec.Multiplexed())
	tr := tickResult{label: label, exit: code, elapsed: time.Since(started), started: started}
	if err != nil {
		s.t.Fatalf("%s tick did not finish within %s: %v", label, tickDeadline, err)
	}
	raw, _ := io.ReadAll(out)
	tr.output = string(raw)
	s.ticks = append(s.ticks, tr)
	return tr
}

func (s *stage) record() formats.FailureRecord {
	s.t.Helper()
	raw := s.readFile(stateDir + "/last-failure.json")
	var rec formats.FailureRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		s.t.Fatalf("last-failure.json does not parse: %v\n%s", err, raw)
	}
	return rec
}

// Every fault this scenario armed, lifted together: toxics, a disabled proxy, both far ends, DNS.
func (s *stage) lift() {
	s.t.Helper()
	for _, p := range []*toxiproxy.Proxy{s.cp, s.storeTox} {
		toxics, err := p.Toxics()
		if err != nil {
			s.t.Fatalf("list toxics on %s: %v", p.Name, err)
		}
		for _, tx := range toxics {
			if err := p.RemoveToxic(tx.Name); err != nil {
				s.t.Fatalf("remove toxic %s from %s: %v", tx.Name, p.Name, err)
			}
		}
		if !p.Enabled {
			if err := p.Enable(); err != nil {
				s.t.Fatalf("enable %s: %v", p.Name, err)
			}
		}
	}
	s.in.setAuthorize(authorizeFault{})
	s.in.setStore(storeFault{})
	s.in.setOnConfig(nil)
	clearDNS(s.t, s.cpHost)
	clearDNS(s.t, s.store)
}

// A reset_peer toxic at toxicity zero never fires but arms SO_LINGER(0), so any close becomes a reset.
func (s *stage) armResets() {
	s.t.Helper()
	if _, err := s.storeTox.AddToxic("arm-reset", "reset_peer", "downstream", 0, toxiAttrs{"timeout": 0}); err != nil {
		s.t.Fatalf("arm resets on %s: %v", s.storeTox.Name, err)
	}
}

// A cut cable: no data either way on either link, nothing closed. Runs off the test goroutine.
func (s *stage) cutLinks() error {
	for _, p := range []*toxiproxy.Proxy{s.cp, s.storeTox} {
		for _, stream := range []string{"upstream", "downstream"} {
			if _, err := p.AddToxic("cut-"+stream, "timeout", stream, 1, toxiAttrs{"timeout": 0}); err != nil {
				return fmt.Errorf("cut %s %s: %w", p.Name, stream, err)
			}
		}
	}
	return nil
}

// --- DNS control --------------------------------------------------------------

type dnsFault struct {
	Rcode   string `json:"rcode,omitempty"`
	Drop    bool   `json:"drop,omitempty"`
	Address string `json:"address,omitempty"`
}

var dnsClient = &http.Client{Timeout: 10 * time.Second}

// Safe from any goroutine, the control plane's handlers included: a failure is reported, not fatal.
func setDNS(t *testing.T, name string, f dnsFault) {
	body, _ := json.Marshal(f)
	if err := dnsCall(http.MethodPut, name, body); err != nil {
		t.Errorf("%v", err)
	}
}

func clearDNS(t *testing.T, name string) {
	if err := dnsCall(http.MethodDelete, name, nil); err != nil {
		t.Errorf("%v", err)
	}
}

func dnsCall(method, name string, body []byte) error {
	req, err := http.NewRequest(method, dnsAPI+"/names/"+name, bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := dnsClient.Do(req)
	if err != nil {
		return fmt.Errorf("DNS control %s %s: %w", method, name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("DNS control %s %s: HTTP %d: %s", method, name, resp.StatusCode, strings.TrimSpace(string(reason)))
	}
	return nil
}

// --- transcripts ----------------------------------------------------------------

const firstLine = `{"type":"user","uuid":"u0","sessionId":%q,"cwd":"/home/tester/work/demo","message":{"role":"user","content":[{"type":"text","text":"start the run"}]}}` + "\n"

// Word-shaped seeded filler: random characters would trip the entropy backstop and redact.
func transcript(seed int64, session string, size int) []byte {
	rng := rand.New(rand.NewSource(seed))
	var b strings.Builder
	b.Grow(size + 1024)
	fmt.Fprintf(&b, firstLine, session)
	for line := 1; b.Len() < size; line++ {
		var text strings.Builder
		for text.Len() < 400 {
			if text.Len() > 0 {
				text.WriteByte(' ')
			}
			for range 3 + rng.Intn(9) {
				text.WriteByte(byte('a' + rng.Intn(26)))
			}
		}
		fmt.Fprintf(&b, `{"type":"assistant","uuid":"a%d","sessionId":%q,"message":{"id":"m%d","model":"claude-opus-5","content":[{"type":"text","text":%q}]}}`+"\n",
			line, session, line, text.String())
	}
	return []byte(b.String())
}
