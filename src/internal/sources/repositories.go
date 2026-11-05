package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// Repositories records, while it is still on disk, which git repository each directory a collected
// session ran in belongs to: one record per repository (its common git dir, so worktrees fold in)
// with its remotes and those directories. Which repositories are one project is the reader's call.
type Repositories struct{}

// Summary is a primitive whose input is what the run's other sources discovered, so it runs after them.
type Summary interface{ summarizesDiscovery() }

func (Repositories) summarizesDiscovery() {}

// repositoryRecord is one repository; the record without a GitDir holds the directories outside git.
type repositoryRecord struct {
	GitDir string `json:"git_dir,omitempty"`
	// MainWorktree is empty for a bare repository, whose checkouts are all linked worktrees.
	MainWorktree string                `json:"main_worktree,omitempty"`
	Remotes      []gitRemote           `json:"remotes,omitempty"`
	Directories  []repositoryDirectory `json:"directories"`
}

type repositoryDirectory struct {
	Path string `json:"path"`
	// Missing: gone from disk (a removed worktree, a remote machine's path), so it was placed by
	// the nearest checkout above it.
	Missing bool `json:"missing,omitempty"`
}

func (Repositories) Discover(req Request) (Discovery, error) {
	d, bucket, ready, err := collectionBucket(req, "repositories")
	if !ready {
		return d, err
	}
	dirs := req.Attribution.attributed()
	if len(dirs) == 0 {
		d.Reason = "no collected session named a working directory"
		return d, nil
	}
	name := "repositories." + bucket.Format("20060102T150405Z") + ".jsonl"
	d.Candidates = []Candidate{{Path: name, RelPath: name, Series: req.Source.ID, Size: req.Source.MaxFileBytes, MTime: bucket,
		Load: func(ctx context.Context) (Payload, error) { return loadRepositories(ctx, req, dirs) },
	}}
	d.Health = Collected
	return d, nil
}

// loadRepositories places each directory in its repository. Paths take the username placeholder the
// manifest's native path takes, so a directory matches the cwd its sessions name.
func loadRepositories(ctx context.Context, req Request, dirs map[string]gitScope) (Payload, error) {
	user := func(p string) string { return formats.ApplyUserPlaceholder(p, req.Username) }
	byGitDir := map[string]*repositoryRecord{}
	for dir, sc := range dirs {
		if err := ctx.Err(); err != nil {
			return Payload{}, err
		}
		rec, seen := byGitDir[sc.common]
		if !seen {
			rec = &repositoryRecord{GitDir: user(sc.common), MainWorktree: user(sc.main)}
			rec.Remotes = req.Attribution.remotesOf(sc.common)
			byGitDir[sc.common] = rec
		}
		_, err := os.Lstat(dir)
		rec.Directories = append(rec.Directories, repositoryDirectory{Path: user(dir), Missing: err != nil})
	}

	recs := make([]*repositoryRecord, 0, len(byGitDir))
	for _, rec := range byGitDir {
		slices.SortFunc(rec.Directories, func(a, b repositoryDirectory) int { return strings.Compare(a.Path, b.Path) })
		recs = append(recs, rec)
	}
	// Outside git last, then by git dir, so an unchanged set encodes to the same bytes.
	slices.SortFunc(recs, func(a, b *repositoryRecord) int {
		switch {
		case a.GitDir == "":
			return 1
		case b.GitDir == "":
			return -1
		}
		return strings.Compare(a.GitDir, b.GitDir)
	})
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, rec := range recs {
		if err := enc.Encode(rec); err != nil {
			return Payload{}, err
		}
	}
	if req.Source.MaxFileBytes > 0 && int64(b.Len()) > req.Source.MaxFileBytes {
		return Payload{}, fmt.Errorf("%w: generated content exceeds file size limit", platform.ErrTooLarge)
	}
	return Payload{Bytes: []byte(b.String())}, nil
}
