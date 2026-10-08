package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Origin says where the remote layer in force came from.
type Origin string

// ErrConfigRejected identifies invalid remote configuration without exposing its contents to telemetry.
var ErrConfigRejected = errors.New("backend: remote configuration rejected")

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

	Effective *config.Effective

	// FetchedAt anchors rejection alerts across restarts; zero means no readable cache.
	FetchedAt time.Time

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

	// Validate resolves a candidate with the local layers before it can replace the working cache.
	Validate func(*config.Document) (*config.Effective, error)
}

// Refresh falls back to the last accepted remote layer when a fetch fails or is rejected.
// Local configuration errors remain fatal; neither fallback nor expiry can repair them.
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
		if localRejection(err) {
			return Remote{Origin: OriginNone, Err: err}
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

	// Older clients may have cached a document that parses but cannot resolve.
	doc, err := config.ParseServedDocument(cached.Config)
	var eff *config.Effective
	if err == nil && o.Validate != nil {
		eff, err = o.Validate(doc)
		if localRejection(err) {
			return Remote{Origin: OriginNone, Err: err}
		}
	}
	if err != nil {
		return Remote{Origin: OriginNone, FetchedAt: cached.FetchedAt,
			Err: unusableCache(fetchErr, fmt.Errorf("%w: %w", ErrConfigRejected, err))}
	}

	return Remote{
		Origin:    OriginCached,
		Doc:       doc,
		Effective: eff,
		FetchedAt: cached.FetchedAt,
		Expired:   cached.Expired(o.Now),
		Err:       fetchErr,
	}
}

// Both causes survive so credential refusals and cached remote rejections remain distinguishable.
func unusableCache(fetchErr, cacheErr error) error {
	return fmt.Errorf("%w; cached remote layer in %s is invalid or unreadable (%w); "+
		"collecting under local config only. Correct the remote configuration or cache read error",
		fetchErr, CacheFile, cacheErr)
}

func localRejection(err error) bool {
	var rejection *config.RejectionError
	return errors.As(err, &rejection) && rejection.Layer != config.LayerRemote
}

// fetch caches only a candidate the caller can resolve.
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
	})
	if err != nil {
		return Remote{}, err
	}

	// Built here, not passed in: every field is a compiled fact, so a caller could vary nothing.
	catalog, err := config.CompiledCatalogReport()
	if err != nil {
		return Remote{}, err
	}
	req := ConfigRequest{
		AgentVersion:   platform.Current().String(),
		ConfigVersions: config.AcceptedConfigVersions,
		Catalog:        catalog,
	}

	f, err := c.FetchConfig(ctx, req)
	if err != nil {
		return Remote{}, err
	}

	var eff *config.Effective
	if o.Validate != nil {
		eff, err = o.Validate(f.Doc)
		if err != nil {
			if localRejection(err) {
				return Remote{}, err
			}
			return Remote{}, fmt.Errorf("%w: %w", ErrConfigRejected, err)
		}
	}

	if err := SaveCache(o.StateDir, Cached{
		Config:    f.Raw,
		FetchedAt: o.Now,
		ExpiresAt: f.ExpiresAt,
	}); err != nil {
		return Remote{}, err
	}

	return Remote{
		Origin:    OriginFetched,
		Doc:       f.Doc,
		Effective: eff,
		FetchedAt: o.Now,
		// A server handing out an already-expired config is misbehaving, and the flag says so.
		Expired: !f.ExpiresAt.IsZero() && o.Now.After(f.ExpiresAt),
	}, nil
}
