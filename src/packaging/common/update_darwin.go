//go:build darwin

package common

import "context"

// Fetch downloads the signed target regardless of the running version. Rename-bridge glue.
func Fetch(ctx context.Context, o Options, selectTarget func(Release) string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(o.Timeout, defaultUpdateTimeout))
	defer cancel()

	repo, release, err := load(ctx)
	if err != nil {
		return nil, "", err
	}
	raw, err := download(repo, release, selectTarget, o)
	return raw, release.Version, err
}
