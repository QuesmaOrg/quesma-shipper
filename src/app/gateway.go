package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/gateway"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// runtimeMaxAge bounds how long a hosted install runs on one fetch of the served config.
const runtimeMaxAge = time.Hour

// hostedShipper runs the ordinary pipeline for each person the gateway spooled telemetry for.
type hostedShipper struct {
	build      Build
	configFile string

	mu       sync.Mutex
	runtimes map[string]cachedRuntime
}

type cachedRuntime struct {
	rt    *Runtime
	built time.Time
}

func (s *hostedShipper) Ship(ctx context.Context, tenant gateway.Tenant, person gateway.Person) error {
	h := Hosted{
		Home:       filepath.Join(person.Dir, "home"),
		StateDir:   filepath.Join(person.Dir, "state"),
		Spool:      person.Spool(),
		ConfigFile: s.configFile,
	}
	if err := platform.EnsureDir(h.Home, 0o700); err != nil {
		return err
	}
	if err := h.Enroll(ctx, tenant.FleetManager, tenant.Grant, person.Label); err != nil {
		return err
	}
	rt, err := s.runtime(ctx, person.Dir, h)
	if err != nil {
		return err
	}
	rep, err := rt.Flush(ctx, false)
	if err != nil {
		s.forget(person.Dir)
		return err
	}
	if rep.Failed+rep.Parked+rep.Skipped > 0 || rep.Truncated {
		return fmt.Errorf("the run left files behind: %d failed, %d parked, %d skipped, truncated=%t",
			rep.Failed, rep.Parked, rep.Skipped, rep.Truncated)
	}
	return nil
}

func (s *hostedShipper) runtime(ctx context.Context, key string, h Hosted) (*Runtime, error) {
	s.mu.Lock()
	cached, ok := s.runtimes[key]
	s.mu.Unlock()
	if ok && time.Since(cached.built) < runtimeMaxAge {
		return cached.rt, nil
	}
	rt, err := h.Runtime(ctx, s.build)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.runtimes[key] = cachedRuntime{rt, time.Now()}
	s.mu.Unlock()
	return rt, nil
}

func (s *hostedShipper) forget(key string) {
	s.mu.Lock()
	delete(s.runtimes, key)
	s.mu.Unlock()
}

// NewGateway assembles the OTLP gateway over the hosted-install pipeline.
func NewGateway(cfg *gateway.Config, build Build, log *slog.Logger) (*gateway.Gateway, error) {
	return gateway.New(cfg, &hostedShipper{build: build, configFile: cfg.ShipperConfig, runtimes: map[string]cachedRuntime{}}, log)
}

// ServeGateway listens until ctx ends, then ships what is spooled.
func ServeGateway(ctx context.Context, configPath string, build Build, log *slog.Logger) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := gateway.ParseConfig(raw)
	if err != nil {
		return err
	}
	gw, err := NewGateway(cfg, build, log)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: gw.Handler(), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	shipped := make(chan struct{})
	go func() { gw.Run(ctx); close(shipped) }()
	log.Info("listening", "address", cfg.Listen, "organizations", len(cfg.Tenants))

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(stop)
	<-shipped
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
