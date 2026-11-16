package gateway

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/otlp"
)

// Shipper runs the upload pipeline over one person's spool. A nil error means every file the
// spool held when the call began is in the sink.
type Shipper interface {
	Ship(ctx context.Context, tenant Tenant, person Person) error
}

type Gateway struct {
	cfg     *Config
	spool   *spool
	shipper Shipper
	log     *slog.Logger
}

const shipWorkers = 8

func New(cfg *Config, shipper Shipper, log *slog.Logger) (*Gateway, error) {
	sp, err := openSpool(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	return &Gateway{cfg: cfg, spool: sp, shipper: shipper, log: log}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/logs", func(w http.ResponseWriter, r *http.Request) { g.receive(w, r, otlp.Logs) })
	mux.HandleFunc("POST /v1/metrics", func(w http.ResponseWriter, r *http.Request) { g.receive(w, r, otlp.Metrics) })
	// Accepted and discarded: an agent configured to export traces must not retry forever.
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, r *http.Request) {
		if g.tenant(r) == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, g.cfg.MaxBodyBytes))
		ok(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return mux
}

func (g *Gateway) tenant(r *http.Request) *Tenant {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !found {
		return nil
	}
	return g.cfg.tenantFor(strings.TrimSpace(token))
}

// An empty body is a valid empty Export*ServiceResponse in both encodings' terms.
func ok(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
}

func (g *Gateway) receive(w http.ResponseWriter, r *http.Request, signal otlp.Signal) {
	tenant := g.tenant(r)
	if tenant == nil {
		// Logged, since a mistyped token otherwise looks like an agent that sends nothing.
		g.log.Warn("unauthorized export", "path", r.URL.Path, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body io.Reader = http.MaxBytesReader(w, r.Body, g.cfg.MaxBodyBytes)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(body)
		if err != nil {
			http.Error(w, "bad gzip body", http.StatusBadRequest)
			return
		}
		// The limit holds after decompression too, or a small body could expand without bound.
		body = io.LimitReader(zr, g.cfg.MaxBodyBytes+1)
	}
	raw, err := io.ReadAll(body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) || int64(len(raw)) > g.cfg.MaxBodyBytes {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	events, dropped, err := otlp.Decode(signal, r.Header.Get("Content-Type"), raw)
	if err != nil {
		g.log.Warn("undecodable export", "organization", tenant.Organization, "path", r.URL.Path, "error", err.Error())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	type group struct {
		st     *personState
		agent  string
		claims []map[string]any
		seen   map[string]bool
		lines  [][]byte
	}
	groups := map[string]*group{}
	var order []*group
	add := func(st *personState, e otlp.Event) {
		gr := groups[st.person.Dir+"\x00"+e.Agent]
		if gr == nil {
			gr = &group{st: st, agent: e.Agent, seen: map[string]bool{}}
			groups[st.person.Dir+"\x00"+e.Agent] = gr
			order = append(order, gr)
		}
		if id := claimID(e.Agent, e.Claims); !gr.seen[id] {
			gr.seen[id] = true
			gr.claims = append(gr.claims, e.Claims)
		}
		gr.lines = append(gr.lines, e.Line)
	}
	resolve := func(keys []string) (*personState, bool) {
		st, err := g.spool.person(tenant, keys)
		if errors.Is(err, errTooManyPeople) {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return nil, false
		}
		if err != nil {
			g.fail(w, tenant, err)
			return nil, false
		}
		return st, true
	}

	// One process exports one request, so an event that does not say who sent it belongs to the
	// one person the rest of the request names for that agent.
	refused, unattributed := 0, 0
	named := map[string]map[*personState]bool{}
	var unnamed []otlp.Event
	for _, e := range events {
		if !tenant.admits(e.Claims) {
			refused++
			continue
		}
		keys := personKeys(e.Agent, e.Claims)
		if len(keys) == 0 {
			unnamed = append(unnamed, e)
			continue
		}
		st, ok := resolve(keys)
		if !ok {
			return
		}
		if named[e.Agent] == nil {
			named[e.Agent] = map[*personState]bool{}
		}
		named[e.Agent][st] = true
		add(st, e)
	}
	for _, e := range unnamed {
		if len(named[e.Agent]) == 1 {
			for st := range named[e.Agent] {
				add(st, e)
			}
			continue
		}
		// Nobody to inherit from: kept under the machine if it is part of a session, else noise.
		if !e.Session {
			unattributed++
			continue
		}
		st, ok := resolve([]string{hostKey(e.Agent, e.Claims)})
		if !ok {
			return
		}
		add(st, e)
	}
	for _, gr := range order {
		if err := g.spool.append(gr.st, gr.agent, gr.claims, gr.lines); err != nil {
			g.fail(w, tenant, err)
			return
		}
	}
	if dropped+refused+unattributed > 0 {
		g.log.Warn("events not stored", "organization", tenant.Organization, "signal", string(signal),
			"unknown_service", dropped, "email_domain_refused", refused, "no_person_no_session", unattributed)
	}
	ok(w, r)
}

// 503 so the exporter retries: the lines are not on disk.
func (g *Gateway) fail(w http.ResponseWriter, tenant *Tenant, err error) {
	g.log.Error("spool write failed", "organization", tenant.Organization, "error", err.Error())
	http.Error(w, "spool unavailable", http.StatusServiceUnavailable)
}

// Run ships on every tick until ctx ends, then once more so a clean stop leaves nothing behind.
func (g *Gateway) Run(ctx context.Context) {
	ticker := time.NewTicker(g.cfg.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.Background(), time.Minute)
			g.Flush(final)
			cancel()
			return
		case <-ticker.C:
			g.Flush(ctx)
		}
	}
}

// Flush ships every dirty person and reports how many failed; those stay dirty.
func (g *Gateway) Flush(ctx context.Context) (failed int) {
	dirty := g.spool.takeDirty()
	work := make(chan *personState)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for range shipWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for st := range work {
				tenant := g.tenantOf(st.person.Organization)
				sealed, err := g.spool.seal(st)
				if err == nil && tenant == nil {
					err = errors.New("the organization is no longer configured")
				}
				if err == nil {
					err = g.shipper.Ship(ctx, *tenant, st.person)
				}
				if err == nil {
					err = g.spool.reap(sealed)
				}
				if err != nil {
					g.log.Error("ship failed", "organization", st.person.Organization, "person", st.person.Dir, "error", err.Error())
					st.mu.Lock()
					st.dirty = true
					st.mu.Unlock()
					mu.Lock()
					failed++
					mu.Unlock()
				}
			}
		}()
	}
	for _, st := range dirty {
		work <- st
	}
	close(work)
	wg.Wait()
	return failed
}

func (g *Gateway) tenantOf(organization string) *Tenant {
	for i := range g.cfg.Tenants {
		if g.cfg.Tenants[i].Organization == organization {
			return &g.cfg.Tenants[i]
		}
	}
	return nil
}
