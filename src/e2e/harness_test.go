// Hermetic end-to-end: a synthetic machine, the real command tree driven through cli.Root, and the
// presigned upload path standing on two httptest servers. No build tag and no Docker, so it runs on
// every commit; what it adds over the unit suite is the wiring between those stages. A run is
// reproducible through a pre-seeded identity, harness-stamped fixture mtimes and a HOME with no
// agent installed; sealed_at stays live and is asserted to fall inside the run, never pinned.
package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/cli"
	seal "github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

// Fixed, because object keys derive from these: a minted-per-run unit would give a different key
// for the same file every time. The published key protects nothing and must never be used for real.
const (
	testInstallID   = "00000000-0000-4000-8000-000000000001"
	testAgeIdentity = "AGE-SECRET-KEY-1JF0Y36Z2RMJNJNN2AYUUF6HMHZVK3FCGK4GUADGRF9M3R57S2UCSDMJWD7"
	testNameKey     = "0101010101010101010101010101010101010101010101010101010101010101"
)

// A manifest carries payload_mtime and a checkout stamps whatever time it happened at, so every
// staged file gets this instead.
var fixtureMTime = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// Path-style, the addressing a self-hosted store uses and the one whose exact-key check has a
// prefix to get wrong.
const vendBucket = "trajectories"

// Domain-separates the v2 signature. Spelled out rather than imported: reusing the client's own
// constant could not notice the client changing it.
const vendPreamble = "trajectory-shipper-upload-authorize-v2\nPOST\n/v2/uploads/authorize\n"

const heartbeatKey = "state/heartbeat.json.age"

// world is one synthetic machine: a HOME the catalog finds sources under, the client's config and
// state, the plane that authorizes its uploads and the store they land in.
type world struct {
	Home     string
	Config   string // XDG_CONFIG_HOME
	State    string // XDG_STATE_HOME
	Identity *age.X25519Identity
	KeyRoot  string

	plane *fakePlane
	store *fakeStore
}

// object is one sealed object, opened.
type object struct {
	Key      string
	Manifest seal.Manifest
	Payload  []byte
}

// --- the fake object store ----------------------------------------------------

// One accepted upload: the key, the provider headers that arrived with it, and the exact bytes.
type storedPut struct {
	Key     string
	Headers map[string]string
	Body    []byte
}

// fakeStore accepts presigned PUTs, signed with a shared HMAC over the key rather than SigV4: an
// HMAC the client cannot compute proves the issued URL reached the store unaltered. It versions
// like a real deployment must: every PUT is kept and the newest write under a key is what is read.
type fakeStore struct {
	server *httptest.Server
	secret []byte

	mu   sync.Mutex
	puts []storedPut
}

func startFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	s := &fakeStore{secret: []byte("e2e-object-store-secret")}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *fakeStore) sign(key string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(key))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *fakeStore) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "the object store accepts PUT only, got "+r.Method, http.StatusMethodNotAllowed)
		return
	}
	prefix := "/" + vendBucket + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.Error(w, "path names no bucket", http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, prefix)
	if !hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(s.sign(key))) {
		http.Error(w, "signature does not cover this key", http.StatusForbidden)
		return
	}

	// A store refuses an expired capability, whatever the bytes behind it are.
	if r.URL.Query().Get("expired") == "1" {
		http.Error(w, "Request has expired", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "short body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if int64(len(body)) != r.ContentLength {
		http.Error(w, fmt.Sprintf("body is %d bytes, Content-Length declared %d",
			len(body), r.ContentLength), http.StatusBadRequest)
		return
	}
	headers := map[string]string{}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") {
			headers[lower] = values[0]
		}
	}
	s.mu.Lock()
	s.puts = append(s.puts, storedPut{Key: key, Headers: headers, Body: body})
	s.mu.Unlock()

	w.Header().Set("ETag", `"`+s.sign(key)[:16]+`"`)
	w.WriteHeader(http.StatusOK)
}

func (s *fakeStore) stored() []storedPut {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storedPut(nil), s.puts...)
}

// mirrorPuts are the trajectory objects, dropping the heartbeat every run writes.
func (s *fakeStore) mirrorPuts() []storedPut {
	var out []storedPut
	for _, p := range s.stored() {
		if strings.Contains(p.Key, "/mirror/") {
			out = append(out, p)
		}
	}
	return out
}

func (s *fakeStore) heartbeats() []storedPut {
	var out []storedPut
	for _, p := range s.stored() {
		if strings.HasSuffix(p.Key, heartbeatKey) {
			out = append(out, p)
		}
	}
	return out
}

// The write count under one key, which is the only thing making "the same file shipped twice"
// observable: the newest version alone cannot tell the two cases apart.
func (s *fakeStore) versions(key string) int {
	n := 0
	for _, p := range s.stored() {
		if p.Key == key {
			n++
		}
	}
	return n
}

// --- the fake control plane ---------------------------------------------------

type authorizeRequest struct {
	WriterID string    `json:"writer_id"`
	IssuedAt time.Time `json:"issued_at"`
	Objects  []struct {
		ObjectID   string            `json:"object_id"`
		Key        string            `json:"key"`
		Size       int64             `json:"size"`
		SourceHash string            `json:"source_hash"`
		Metadata   map[string]string `json:"metadata"`
	} `json:"objects"`
}

// fakePlane authenticates the device signature before reading a single object field, which is the
// order the real service is specified in.
type fakePlane struct {
	server *httptest.Server
	store  *fakeStore
	pub    ed25519.PublicKey

	mu sync.Mutex
	// status is what the next authorize answers. 200 issues tickets.
	status int
	// ttl is how long an issued ticket lives.
	ttl time.Duration
	// How many of the next authorizations issue already-expired tickets, so the client's single
	// reauthorization can be driven from the server side.
	staleBatches int
	// The install id every ticket URL is rewritten to name, signature recomputed, so only the
	// client's exact-key check can refuse it.
	misdirect string
	// issued counts authorizations answered, so every minted ticket id is distinct.
	issued int
	// batches records the keys of each authorize call, in call order.
	batches [][]string
	// writers records every distinct writer_id seen.
	writers map[string]bool
	// faults are protocol invariants the client broke.
	faults []string
}

func startFakePlane(t *testing.T, store *fakeStore, pub ed25519.PublicKey) *fakePlane {
	t.Helper()
	p := &fakePlane{store: store, pub: pub, status: http.StatusOK, ttl: 10 * time.Minute,
		writers: map[string]bool{}}
	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

func (p *fakePlane) setStatus(status int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
}

func (p *fakePlane) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v2/uploads/authorize" {
		// Including the config fetch: an unreachable config is ordinary, and the run continues on
		// the layers it already has.
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := p.verify(r.Header.Get("Authorization"), body); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	p.mu.Lock()
	status, ttl := p.status, p.ttl
	stale := p.staleBatches > 0
	if stale {
		p.staleBatches--
		ttl = -time.Second
	}
	// Ticket ids are the server's to mint and must not repeat across authorizations.
	seq := p.issued
	p.issued++
	p.mu.Unlock()
	if status != http.StatusOK {
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "30")
		}
		http.Error(w, fmt.Sprintf("the control plane was told to answer %d", status), status)
		return
	}

	var req authorizeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "undecodable request: "+err.Error(), http.StatusBadRequest)
		return
	}

	tickets := make([]map[string]any, 0, len(req.Objects))
	keys := make([]string, 0, len(req.Objects))
	expires := time.Now().Add(ttl).UTC()
	for i, obj := range req.Objects {
		p.check(obj.ObjectID, obj.Key, obj.Size, obj.SourceHash, obj.Metadata)
		keys = append(keys, obj.Key)
		ticketID := fmt.Sprintf("ticket-%d-%d", seq, i)
		headers := map[string]string{
			"x-amz-meta-source-hash": obj.SourceHash,
			"x-amz-meta-ticket-id":   ticketID,
			"x-amz-tagging":          taggingFor(obj.Key),
		}
		for name, value := range obj.Metadata {
			headers["x-amz-meta-"+name] = value
		}
		tickets = append(tickets, map[string]any{
			"ticket_id":             ticketID,
			"object_id":             obj.ObjectID,
			"method":                "PUT",
			"url":                   p.ticketURL(obj.Key, stale),
			"expires_at":            expires.Format(time.RFC3339Nano),
			"required_headers":      headers,
			"content_length":        obj.Size,
			"content_length_signed": true,
		})
	}

	p.mu.Lock()
	p.batches = append(p.batches, keys)
	p.writers[req.WriterID] = true
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tickets": tickets})
}

// verify authenticates the device before any object field is read.
func (p *fakePlane) verify(header string, body []byte) error {
	_, rest, ok := strings.Cut(header, "sig=")
	if !strings.HasPrefix(header, "Shipper-Device org=default, install=") || !ok {
		return fmt.Errorf("authorization header is not a device signature: %q", header)
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	if err != nil {
		return fmt.Errorf("device signature is not base64: %w", err)
	}
	if !ed25519.Verify(p.pub, append([]byte(vendPreamble), body...), sig) {
		return fmt.Errorf("device signature does not cover the preamble and this body")
	}
	return nil
}

// Faults are collected and reported at the end of the test naming the object: answering 400 here
// would surface as a generic upload failure instead.
func (p *fakePlane) check(objectID, key string, size int64, sourceHash string, md map[string]string) {
	fault := func(format string, args ...any) {
		p.mu.Lock()
		p.faults = append(p.faults, fmt.Sprintf(format, args...))
		p.mu.Unlock()
	}
	if objectID == "" {
		fault("object under key %s carries no object_id", key)
	}
	if size <= 0 {
		fault("object %q declares size %d", objectID, size)
	}
	if len(sourceHash) != 64 || strings.ToLower(sourceHash) != sourceHash {
		fault("object %q declares source_hash %q, want 64 lowercase hex", objectID, sourceHash)
	}
	for _, banned := range []string{"source-hash", "ticket-id"} {
		if _, present := md[banned]; present {
			fault("object %q requested server-derived metadata %q", objectID, banned)
		}
	}
	if !strings.Contains(key, "install="+testInstallID) {
		fault("object %q names key %q, outside this install's root", objectID, key)
	}
}

func (p *fakePlane) ticketURL(key string, stale bool) string {
	if p.misdirect != "" {
		key = strings.Replace(key, testInstallID, p.misdirect, 1)
	}
	url := p.store.server.URL + "/" + vendBucket + "/" + canonicalKeyPath(key) +
		"?sig=" + p.store.sign(key)
	if stale {
		url += "&expired=1"
	}
	return url
}

func (p *fakePlane) authorizeBatches() [][]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]string(nil), p.batches...)
}

func (p *fakePlane) assertClean(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.faults {
		t.Errorf("authorization request broke a protocol invariant: %s", f)
	}
}

// The writer id is minted once per process, so a run presenting two reports itself as two machines.
func (p *fakePlane) assertOneWriter(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.writers) != 1 {
		t.Errorf("one run presented %d writer ids, want exactly one", len(p.writers))
	}
}

// The server-derived tag: the client cannot request one, so it comes from the key's container.
func taggingFor(key string) string {
	if strings.Contains(key, "/mirror/") {
		return "class=trajectory"
	}
	return "class=context"
}

// The server's escaped spelling of an object key, written out rather than imported: the exact-key
// check is only meaningful when the two sides derive it independently.
func canonicalKeyPath(key string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~', c == '/':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}

// --- staging the machine ------------------------------------------------------

// stageWorld points the client at the machine through the environment, not flags, because the
// catalog resolves its roots through HOME. It is the enrolled shape, the only one that can upload.
func stageWorld(t *testing.T, opts ...func(*world)) *world {
	t.Helper()
	w := stageBareWorld(t)
	seedIdentity(t, w)

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	w.store = startFakeStore(t)
	w.plane = startFakePlane(t, w.store, pub)
	seedEnrollment(t, w, w.plane.server.URL, priv)

	writeConfig(t, w, "")
	for _, opt := range opts {
		opt(w)
	}
	return w
}

// Directories and environment only: no identity, no enrollment, no config. The world `enroll`
// meets, and with no enrollment record there is no route to any destination at all.
func stageBareWorld(t *testing.T) *world {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{
		Home:    filepath.Join(root, "home"),
		Config:  filepath.Join(root, "config"),
		State:   filepath.Join(root, "state"),
		KeyRoot: "v1/organization=default/install=" + testInstallID,
	}
	for _, dir := range []string{w.Home, w.Config, w.State} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("HOME", w.Home)
	t.Setenv("USERPROFILE", w.Home)
	t.Setenv("APPDATA", filepath.Join(w.Home, "AppData", "Roaming"))
	t.Setenv("XDG_CONFIG_HOME", w.Config)
	t.Setenv("XDG_STATE_HOME", w.State)
	return w
}

func seedIdentity(t *testing.T, w *world) {
	t.Helper()
	id, err := age.ParseX25519Identity(testAgeIdentity)
	if err != nil {
		t.Fatalf("the test identity does not parse: %v", err)
	}
	w.Identity = id

	dir := filepath.Join(w.State, "trajectory-shipper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	unit := map[string]any{
		"identity_schema": 1,
		"install_id":      testInstallID,
		"age_identity":    testAgeIdentity,
		"age_recipient":   id.Recipient().String(),
		"name_key":        testNameKey,
		"created_at":      fixtureMTime.Format(time.RFC3339),
	}
	raw, err := json.Marshal(unit)
	if err != nil {
		t.Fatal(err)
	}
	// 0600: Load refuses a unit any wider, and that refusal is exercised here on purpose.
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 0600, because LoadEnrollment refuses anything wider and the file holds a private signing key.
func seedEnrollment(t *testing.T, w *world, endpoint string, deviceKey ed25519.PrivateKey) {
	t.Helper()
	record := map[string]any{
		"enrollment_schema": 2,
		"install_id":        testInstallID,
		"organization":      "default",
		"endpoint":          endpoint,
		"device_key":        base64.StdEncoding.EncodeToString(deviceKey),
		"enrolled_at":       fixtureMTime.Format(time.RFC3339),
	}
	raw, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.State, "trajectory-shipper", "enrollment.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeConfig writes the client's own config; extra is appended verbatim, which is how a test says
// "and this source is disabled" without a second helper.
func writeConfig(t *testing.T, w *world, extra string) {
	t.Helper()
	if w.store == nil {
		t.Fatal("writeConfig needs a staged object store; this world has none")
	}
	dir := filepath.Join(w.Config, "trajectory-shipper")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "config_version: 1\n" +
		// Deliberately a block an older build wrote: it selects nothing now, and every verb has to
		// keep working over it rather than failing to parse after an update.
		"send:\n  sink: file\n  path: /var/tmp/trajectory-archive\n" +
		// Pinned because the store speaks loopback HTTP, which only a configured entry may admit.
		"upload_targets:\n" +
		"  - origin: " + w.store.server.URL + "\n" +
		"    addressing: path-style\n" +
		"    path_prefix: /" + vendBucket + "\n" +
		"    allow_loopback_http: true\n" +
		// The 64-file default would truncate a grown fixture, and read as a collection bug.
		"max_files_per_run: 10000\n" +
		extra
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The install that enrolled and never configured upload_targets, running unpinned: tickets decide
// the destination, https only.
func writeConfigWithoutUploadTargets(t *testing.T, w *world) {
	t.Helper()
	dir := filepath.Join(w.Config, "trajectory-shipper")
	body := "config_version: 1\nmax_files_per_run: 10000\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// run executes one command through the real command tree and returns its output.
func run(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root := cli.Root(app.Build{Version: "e2e"}, &out, &out)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("shipper %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func runOneShot(t *testing.T, flags ...string) string {
	t.Helper()
	return run(t, append([]string{"run", "--once"}, flags...)...)
}

// runExpectingFailure is for the paths whose whole point is a non-zero exit.
func runExpectingFailure(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := cli.Root(app.Build{Version: "e2e"}, &out, &out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func runUntilCancelled(t *testing.T, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	root := cli.Root(app.Build{Version: "e2e"}, &out, &out)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	return out.String(), err
}

func runOneShotExpectingFailure(t *testing.T, flags ...string) (string, error) {
	t.Helper()
	return runExpectingFailure(t, append([]string{"run", "--once"}, flags...)...)
}

// summary parses the run's own counters ("shipped 4 unchanged 0 ..."). Never count objects in the
// store instead: a re-shipped file lands under the same key, so the key set is identical whether
// the run sent everything or nothing, which is exactly what change detection is about.
func summary(t *testing.T, out string) map[string]int {
	t.Helper()
	counts := map[string]int{}
	fields := strings.Fields(out)
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case "shipped", "unchanged", "skipped", "parked", "failed":
			var n int
			if _, err := fmt.Sscanf(fields[i+1], "%d", &n); err == nil {
				counts[fields[i]] = n
			}
		}
	}
	if len(counts) == 0 {
		t.Fatalf("no run summary in the output:\n%s", out)
	}
	return counts
}

// The source id of everything the run actually sent, per file. A global "shipped 0" would be both
// too strict and too vague, because the generated project-map sidecar can legitimately change when
// nothing was collected. Change-detection assertions must feed this the run log via shippedFromLog:
// the console truncates to 32 per-file lines, so parsing it directly is only for console tests.
func shippedSources(out string) []string {
	var out2 []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || !strings.HasPrefix(fields[0], "[") {
			continue
		}
		for _, f := range fields[2:] {
			if f == "shipped" {
				out2 = append(out2, fields[1])
				break
			}
		}
	}
	return out2
}

// The log is the complete record, bounded by neither the console budget nor --quiet. A missing
// file fails rather than returning empty, because that is what the tests using it check.
func runLogLines(t *testing.T, w *world) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.State, "trajectory-shipper", "last-sync.log"))
	if err != nil {
		t.Fatalf("the sync wrote no run log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// shippedSources over the untruncated run log, so growing a fixture past the console's 32-line
// budget can never turn "nothing re-shipped" into "nothing was printed".
func shippedFromLog(t *testing.T, w *world) []string {
	t.Helper()
	return shippedSources(strings.Join(runLogLines(t, w), "\n"))
}

func countOf(list []string, want string) int {
	n := 0
	for _, v := range list {
		if v == want {
			n++
		}
	}
	return n
}

// collect opens the current version of every object, sorted by key; the write history behind it is
// fakeStore.versions. Age is nondeterministic, so only the manifest and payload inside are stable.
func collect(t *testing.T, w *world) []object {
	t.Helper()
	current := map[string]storedPut{}
	for _, put := range w.store.stored() {
		current[put.Key] = put
	}
	objects := make([]object, 0, len(current))
	for key, put := range current {
		m, payload, err := seal.Open(put.Body, w.Identity)
		if err != nil {
			t.Fatalf("%s: open: %v", key, err)
		}
		objects = append(objects, object{Key: key, Manifest: m, Payload: payload})
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects
}

// Drops the heartbeat and anything else under state/, which is written on every run regardless.
func mirrorObjects(objs []object) []object {
	var out []object
	for _, o := range objs {
		if strings.Contains(o.Key, "/mirror/") {
			out = append(out, o)
		}
	}
	return out
}

func bySourceID(objs []object, id string) []object {
	var out []object
	for _, o := range objs {
		if o.Manifest.SourceID == id {
			out = append(out, o)
		}
	}
	return out
}

// --- staging fixtures ---------------------------------------------------------

// stageFile writes one file into the synthetic HOME and stamps the fixed mtime on it.
func stageFile(t *testing.T, w *world, rel, content string) string {
	t.Helper()
	full := filepath.Join(w.Home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	touch(t, full)
	return full
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.Chtimes(path, fixtureMTime, fixtureMTime); err != nil {
		t.Fatal(err)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Later than the fixture's on purpose: keeping the old timestamp would test the content hash
	// alone, and the mtime pre-filter is part of what runs.
	later := fixtureMTime.Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// A new mtime on every staged file without changing a byte: the case that decides whether change
// detection is correct.
func touchEverything(t *testing.T, w *world) {
	t.Helper()
	later := fixtureMTime.Add(2 * time.Hour)
	err := filepath.Walk(w.Home, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		return os.Chtimes(p, later, later)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Must stay in step with cursorjoin.DBCandidates: if that moves, the Cursor fixtures stop being
const cursorStateDB = "Cursor/User/globalStorage/state.vscdb"

// found and TestCursorPairYieldsOneDerivedObject fails rather than passing on the raw path.
func cursorStatePath() string {
	if runtime.GOOS == "darwin" {
		return "Library/Application Support/" + cursorStateDB
	}
	if runtime.GOOS == "windows" {
		return "AppData/Roaming/" + cursorStateDB
	}
	return ".config/" + cursorStateDB
}

func ensureDir(path string) error { return os.MkdirAll(path, 0o700) }
