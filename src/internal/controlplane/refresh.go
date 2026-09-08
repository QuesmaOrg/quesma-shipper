package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Origin says where the remote layer in force came from.
type Origin string

const (
	// OriginFetched is a fresh fetch.
	OriginFetched Origin = "fetched"

	// OriginCached is the last config fetched, reused because this run could not get a new one.
	OriginCached Origin = "cached"

	// OriginNone means there is no remote layer at all: an ordinary state, not a degraded one,
	// because the local layers are a complete configuration.
	OriginNone Origin = "none"
)

// Remote is the resolved remote layer: the org's config, served over TLS by the control
// plane this install enrolled with. It enters the resolver as an ordinary document layer.
type Remote struct {
	Origin Origin

	// Doc is the served config. Nil when Origin is OriginNone.
	Doc *config.Document

	// Expired is true when a cached config past its expiry is in use; it does not stop collection.
	Expired bool

	// Err is why a fetch did not happen or was refused. Reported, never swallowed: a quiet fallback
	// leaves an operator believing a config change took effect.
	Err error
}

// RefreshOptions configures a refresh.
type RefreshOptions struct {
	Enrollment *Enrollment
	StateDir   string
	Now        time.Time

	// Offline skips the network and resolves from the cache: what the read-only verbs use, since
	// `config show` must explain the config in force without a round-trip that changes it.
	Offline bool

	HTTPClient *http.Client
}

// Refresh produces the remote layer for one run: a fetch that parses wins and is cached, and ANY
// failure to obtain a config this client accepts falls back to the last one it did fetch, whose
// expiry does not stop collection. A transcript missed before its source store's reaper runs is gone.
func Refresh(ctx context.Context, o RefreshOptions) Remote {
	if o.Enrollment == nil || o.Enrollment.Endpoint == "" {
		return Remote{Origin: OriginNone}
	}

	var fetchErr error
	if !o.Offline {
		fetched, err := fetch(ctx, o)
		if err == nil {
			return fetched
		}
		fetchErr = err
	} else {
		fetchErr = errors.New("backend: offline, resolving from the cached config")
	}

	cached, err := LoadCache(o.StateDir)
	if err != nil {
		// No usable cache and no fetch: no remote layer, and the run still collects locally.
		return Remote{Origin: OriginNone, Err: unusableCache(fetchErr, err)}
	}

	// Reparsed on load: a cache that no longer parses is damage like any other unreadable file, so
	// it degrades to local config instead of stopping the run.
	doc, err := config.ParseServedDocument(cached.Config)
	if err != nil {
		return Remote{Origin: OriginNone, Err: unusableCache(fetchErr, err)}
	}

	return Remote{
		Origin:  OriginCached,
		Doc:     doc,
		Expired: cached.Expired(o.Now),
		Err:     fetchErr,
	}
}

// unusableCache explains a run with neither a fresh config nor a usable cached one. Both halves:
// a cache that no longer parses is a file to delete, unguessable from "backend unreachable".
// Both errors are non-nil by construction: Refresh reaches the cache only after a fetch failure
// (or the offline sentinel), and calls this only on a cache error.
func unusableCache(fetchErr, cacheErr error) error {
	return fmt.Errorf("%w; the cached config is unusable (%v); collecting under local config only. "+
		"Delete %s to clear it", fetchErr, cacheErr, CacheFile)
}

// fetch does one round-trip and caches what it gets.
func fetch(ctx context.Context, o RefreshOptions) (Remote, error) {
	deviceKey, err := o.Enrollment.PrivateKey()
	if err != nil {
		return Remote{}, err
	}

	c, err := New(Options{
		Endpoint:     o.Enrollment.Endpoint,
		InstallID:    o.Enrollment.InstallID,
		Organization: o.Enrollment.Organization,
		DeviceKey:    deviceKey,
		HTTPClient:   o.HTTPClient,
	})
	if err != nil {
		return Remote{}, err
	}

	// Built here, not passed in: both fields are compiled facts, so a caller could vary nothing.
	req := ConfigRequest{
		AgentVersion:   platform.Current().String(),
		ConfigVersions: config.AcceptedConfigVersions,
	}

	f, err := c.FetchConfig(ctx, req)
	if err != nil {
		return Remote{}, err
	}

	if err := SaveCache(o.StateDir, Cached{
		Config:    f.Raw,
		FetchedAt: o.Now,
		ExpiresAt: f.ExpiresAt,
	}); err != nil {
		return Remote{}, err
	}

	return Remote{
		Origin: OriginFetched,
		Doc:    f.Doc,
		// A server handing out an already-expired config is misbehaving, and the flag says so.
		Expired: !f.ExpiresAt.IsZero() && o.Now.After(f.ExpiresAt),
	}, nil
}
