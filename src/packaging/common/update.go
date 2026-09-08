// Package common contains the platform-independent release and service lifecycle.
// Every operation starts from the embedded root and keeps no trusted metadata or target cache.
package common

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/theupdateframework/go-tuf/v2/metadata/config"
	tufupdater "github.com/theupdateframework/go-tuf/v2/metadata/updater"
)

const (
	defaultCheckTimeout  = 2 * time.Minute
	defaultUpdateTimeout = 5 * time.Minute
)

//go:embed roots/1.root.json
var trustedRoot []byte

const baseURL = "https://updates.quesma.dev"

type Release struct {
	Version     string            `json:"version"`
	PublishedAt time.Time         `json:"published_at"`
	Targets     map[string]string `json:"targets"`
}

// Options carries what a caller knows; zero timeouts get safe defaults.
type Options struct {
	Current string
	Timeout time.Duration
	Out     io.Writer
}

// Result says what Update did.
type Result struct {
	Updated  bool
	From, To string
}

// Check verifies the current TUF repository and reports its signed release.
func Check(ctx context.Context, o Options) (latest string, publishedAt time.Time, available bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(o.Timeout, defaultCheckTimeout))
	defer cancel()

	_, release, err := load(ctx)
	if err != nil {
		return "", time.Time{}, false, err
	}
	return release.Version, release.PublishedAt, newer(release.Version, o.Current), nil
}

// Update replaces this binary only when the signed release is newer than o.Current.
func Update(ctx context.Context, o Options, selectTarget func(Release) string,
	applyTarget func([]byte, string) error) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(o.Timeout, defaultUpdateTimeout))
	defer cancel()

	repo, release, err := load(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{From: o.Current, To: release.Version}
	if !newer(release.Version, o.Current) {
		return res, nil
	}

	target := selectTarget(release)
	if target == "" {
		return res, fmt.Errorf("newer release %s has no binary for %s/%s, so this installation stays on %s",
			release.Version, runtime.GOOS, runtime.GOARCH, o.Current)
	}
	info, err := repo.GetTargetInfo(target)
	if err != nil {
		return res, fmt.Errorf("finding %s: %w", target, err)
	}
	progress(o.Out, "downloading shipper %s", release.Version)
	_, binary, err := repo.DownloadTarget(info, "", "")
	if err != nil {
		return res, fmt.Errorf("downloading %s: %w", target, err)
	}
	if err := applyTarget(binary, release.Version); err != nil {
		return res, fmt.Errorf("installing update: %w", err)
	}
	res.Updated = true
	return res, nil
}

func load(ctx context.Context) (*tufupdater.Updater, Release, error) {
	cfg, err := config.New(strings.TrimRight(baseURL, "/")+"/metadata", trustedRoot)
	if err != nil {
		return nil, Release{}, fmt.Errorf("configuring TUF: %w", err)
	}
	cfg.RemoteTargetsURL = strings.TrimRight(baseURL, "/") + "/targets"
	cfg.DisableLocalCache = true
	client := &http.Client{Transport: contextTransport{ctx: ctx}}
	if err := cfg.SetDefaultFetcherHTTPClient(client); err != nil {
		return nil, Release{}, fmt.Errorf("configuring TUF transport: %w", err)
	}
	repo, err := tufupdater.New(cfg)
	if err != nil {
		return nil, Release{}, fmt.Errorf("loading embedded TUF root: %w", err)
	}
	if err := repo.Refresh(); err != nil {
		return nil, Release{}, fmt.Errorf("refreshing TUF metadata: %w", err)
	}
	info, err := repo.GetTargetInfo("release.json")
	if err != nil {
		return nil, Release{}, fmt.Errorf("finding release.json: %w", err)
	}
	_, raw, err := repo.DownloadTarget(info, "", "")
	if err != nil {
		return nil, Release{}, fmt.Errorf("downloading release.json: %w", err)
	}
	var release Release
	if err := json.Unmarshal(raw, &release); err != nil {
		return nil, Release{}, fmt.Errorf("parsing release.json: %w", err)
	}
	if _, err := semver.NewVersion(release.Version); err != nil {
		return nil, Release{}, fmt.Errorf("release.json has invalid version %q: %w", release.Version, err)
	}
	return repo, release, nil
}

// contextTransport gives go-tuf's HTTP fetcher the operation's cancellation and deadline.
type contextTransport struct{ ctx context.Context }

func (t contextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return http.DefaultTransport.RoundTrip(req.Clone(t.ctx))
}

func newer(latest, current string) bool {
	l, err := semver.NewVersion(latest)
	if err != nil {
		return false
	}
	c, err := semver.NewVersion(current)
	if err != nil {
		return false
	}
	return l.GreaterThan(c)
}

func timeoutOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func progress(w io.Writer, format string, args ...any) {
	if w != nil {
		fmt.Fprintf(w, format+"\n", args...)
	}
}
