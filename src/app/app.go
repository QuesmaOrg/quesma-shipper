// Package app is the composition root: the only layer allowed to know which adapter is which
// (presigned PUT uploads, the Cursor join, launchd supervision). Assembly lives here, printing
// lives in cli: a function here returns a value or an error, never a rendered line.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/engine"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/identity"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/sources"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/accountprobe"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms/cursorjoin"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

// Runtime is everything a flush needs, assembled once.
type Runtime struct {
	eff  *config.Effective
	unit *identity.Unit

	// upload is the one write path; uploadErr is held rather than returned so `preview` still
	// runs on an install that cannot authorize anything.
	upload    engine.UploadPort
	uploadErr error

	log   *auditlog.Log
	build Build

	// recipients is built once so every sealed object is encrypted to the same set.
	recipients []age.Recipient

	// remote is the refresh outcome, kept so a verb can report a fallback.
	remote controlplane.Remote

	// env expands an enricher's declared database candidates, with the same rules catalog roots use.
	env sources.Env

	// progress is the per-file streaming hook a verb may register before flushing; rendering is CLI-owned.
	// OnProgress is a per-file callback for subsequent flushes. Nil (the default) is silent.
	OnProgress formats.Progress

	// onLocked is called once a flush holds the store lock. See OnLocked.
	// OnLocked runs once a flush holds the store lock. The lock is non-blocking, so a verb resets a
	// per-run artifact from here, not at startup, where it would reset another's.
	OnLocked func()

	// runID and lastCrash come from the CLI's crash journal; audit entries and heartbeats carry them.
	runID     string
	lastCrash *formats.LastCrash

	// OnCrashShipped fires once, when a heartbeat CARRYING the crash report reached the sink; the
	// heartbeat fails open, so nothing weaker proves delivery.
	OnCrashShipped func()
}

// The run id lands on every audit entry and heartbeat; the previous run's death rides one out.
func (r *Runtime) SetRunInfo(runID string, lastCrash *formats.LastCrash) {
	r.runID, r.lastCrash = runID, lastCrash
	r.log.SetRunID(runID)
}

func New(build Build) (*Runtime, error) {
	// Cancellable, so Ctrl-C during the startup config fetch stops it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The collecting verbs refresh the remote layer first; a failed refresh does not stop the run.
	eff, paths, remote, err := ResolveOnline(ctx)
	if err != nil {
		return nil, err
	}
	return NewFrom(build, eff, paths, remote)
}

// NewFrom builds the runtime from an ALREADY resolved config, so a verb that has resolved once
// (doctor) does not pay for a second config fetch.
func NewFrom(
	build Build,
	eff *config.Effective,
	paths config.Paths,
	remote controlplane.Remote,
) (*Runtime, error) {
	unit, err := identity.Load(paths.StateDir)
	if err != nil {
		return nil, fmt.Errorf("%w\n\nRun `shipper login <token>` first (or `shipper local-dev` without a control\n"+
			"plane): an install needs an identity before it can ship", err)
	}

	recipients, err := recipientsFor(eff, unit)
	if err != nil {
		return nil, err
	}
	log, err := auditlog.Open(paths.StateDir)
	if err != nil {
		return nil, err
	}

	// Held rather than returned so `preview` keeps working on an install that has no control
	// plane. The port stays interface-typed and is assigned only on success: a failed *vendPort
	// would box a typed nil and panic on first use instead of reporting uploadErr.
	var up engine.UploadPort
	port, upErr := newUploadPort(paths.StateDir, eff)
	if upErr == nil {
		up = port
	}

	env, err := sources.OSEnv()
	if err != nil {
		return nil, err
	}
	return &Runtime{eff: eff, unit: unit, upload: up, uploadErr: upErr, log: log, build: build,
		remote: remote, env: env, recipients: recipients}, nil
}

// recipientsFor composes the encryption set from the identity unit and the resolved config:
// refusing beats sealing to fewer readers than the operator configured.
func recipientsFor(eff *config.Effective, unit *identity.Unit) ([]age.Recipient, error) {
	var out []age.Recipient
	if eff.IncludeInstallRecipient {
		out = append(out, unit.Recipient())
	}
	for _, s := range eff.AdditionalRecipients {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("encryption.additional_recipients: %q is not an age X25519 recipient: %w", s, err)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("the recipient set is empty: policy withheld the install recipient and no additional recipients are configured")
	}
	return out, nil
}

func (r *Runtime) options(dryRun bool) engine.Options {
	return engine.Options{
		Plan:       planFor(r.eff),
		Identity:   r.unit,
		Upload:     r.upload,
		Log:        r.log,
		Registry:   sources.NewRegistry(),
		Enrichers:  Enrichers(),
		Env:        r.env,
		Recipients: r.recipients,
		DryRun:     dryRun,
		Client:     clientBlock(),
		Progress:   r.OnProgress,
		RunID:      r.runID,
	}
}

// Enrichers is the compiled enricher registry, shared with doctor so "in this build" cannot
// drift from what the engine runs. Config can disable an entry; no layer can add one.
func Enrichers() *transforms.Registry {
	return transforms.NewRegistry(cursorjoin.New(),
		accountprobe.NewClaude(), accountprobe.NewCodex(), accountprobe.NewCursor())
}

// Flush opens the store, runs once, and closes. Every path goes through here, so all of them
// share one flock and cannot interleave.
func (r *Runtime) Flush(ctx context.Context, dryRun bool) (formats.Report, error) {
	return r.flushWith(ctx, dryRun, false)
}

// Drain flushes everything pending on an ephemeral host, bounded by the deadline rather than by
// max_files_per_run: the per-run bound is the wrong limit, and the deadline bounds a stuck hook.
func (r *Runtime) Drain(ctx context.Context) (formats.Report, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, r.eff.DrainDeadline)
	defer cancel()

	rep, err := r.flushWith(ctx, false, true)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		// Partial, not failed: files commit one at a time, so whatever got through is shipped.
		return rep, false, nil
	}
	if err != nil {
		return rep, false, err
	}
	return rep, !rep.Truncated, nil
}

func (r *Runtime) flushWith(ctx context.Context, dryRun, unbounded bool) (formats.Report, error) {
	store, err := engine.Open(r.eff.StateDir, r.unit.InstallID.String())
	if err != nil {
		return formats.Report{}, err
	}
	defer store.Close()
	if r.OnLocked != nil {
		r.OnLocked()
	}

	// Raised at the one moment it matters; a preview never reaches here with it set.
	if !dryRun && r.uploadErr != nil {
		return formats.Report{}, r.uploadErr
	}

	o := r.options(dryRun)
	o.Unbounded = unbounded
	o.Heartbeat = r.WriteHeartbeat
	rep, err := engine.Run(ctx, store, o)

	// Stamped even when the run shipped nothing: the marker answers "is the agent running at all",
	// which the fingerprint document cannot.
	if !dryRun {
		if markErr := packaging.RecordRun(r.eff.StateDir, time.Now()); markErr != nil && err == nil {
			// A missing marker makes `status` report NEVER on a healthy install.
			fmt.Fprintf(os.Stderr, "warning: could not stamp the last-run marker: %v\n", markErr)
		}
	}
	return rep, err
}

// WriteHeartbeat publishes discovery health as an install-owned state object: under state/ but
// inside the install prefix, so one erasure sweep takes it too, and carrying no transcript bytes.
// `doctor` probes the write path with it, as it is the only state object the protocol authorizes.
func (r *Runtime) WriteHeartbeat(ctx context.Context, rep formats.Report) error {
	return r.writeHeartbeat(ctx, rep, true)
}

// Doctor's write-path probe: same build, authorization and PUT, but it leaves the local mirror
// alone. Doctor collects nothing, so mirroring its all-zero counters would erase the record of the
// last real flush -- the very thing doctor reads.
func (r *Runtime) ProbeHeartbeat(ctx context.Context, rep formats.Report) error {
	return r.writeHeartbeat(ctx, rep, false)
}

func (r *Runtime) writeHeartbeat(ctx context.Context, rep formats.Report, mirror bool) error {
	hb := engine.Build(engine.Input{
		OrganizationID: r.eff.OrganizationID,
		InstallID:      r.unit.InstallID.String(),
		ClientVersion:  r.build.Version,
		ConfigVersion:  r.eff.ConfigVersion,
		ConfigExpired:  r.eff.ConfigExpired,
		RunID:          r.runID,
		Report:         rep,
		Now:            time.Now().UTC(),
		// The crash comes from this process reading the journal; the failures come off disk,
		// written by whichever earlier run could not upload them itself.
		FailureRecord: r.failureRecord(),
	})
	body, err := hb.Encode()
	if err != nil {
		return err
	}

	// Mirrored in the clear (counts and versions, never payload bytes) so `shipper doctor` needs
	// no network call. Best-effort: a reporting nicety must never fail a flush.
	if mirror {
		_ = platform.WriteAtomic(filepath.Join(r.eff.StateDir, engine.Name), body, 0o600)
	}

	// The resolved organization, not a literal: the heartbeat has to land in the same subtree as
	// its mirror objects, or one erasure sweep would miss it.
	key, err := formats.StateKey(r.eff.OrganizationID, r.unit.InstallID.String(), engine.Name+".age")
	if err != nil {
		return err
	}
	sealed, err := transforms.Seal(transforms.Manifest{
		ManifestVersion: transforms.ManifestVersion,
		OrganizationID:  r.eff.OrganizationID,
		InstallID:       r.unit.InstallID.String(),
		SourceID:        "heartbeat",
		NativePath:      engine.Name,
		Gather:          "metadata_only",
		ArtifactClass:   "context",
		SourceHash:      transforms.Hash(body),
		SealedAt:        time.Now().UTC().Format(time.RFC3339),
		ShapeSniff:      string(formats.SniffOK),
		// The same config fields every mirror manifest carries, so no reader special-cases this one.
		ConfigVersion: r.eff.ConfigVersion,
		ConfigExpired: r.eff.ConfigExpired,
		Client:        clientBlock(),
		RunID:         r.runID,
	}, body, r.recipients)
	if err != nil {
		return err
	}

	// The assembly failure first: a nil port and a recorded uploadErr are the same condition.
	if r.uploadErr != nil {
		return r.uploadErr
	}
	// The heartbeat goes down the trajectory path exactly: one authorization, the same validation,
	// the same PUT. No second way to reach the store, which a revocation would have to learn about.
	outcomes := r.upload.AuthorizeAndUpload(ctx, []engine.PreparedObject{{
		ObjectID:   "heartbeat",
		Key:        key,
		Body:       sealed,
		SourceHash: transforms.Hash(body),
		Metadata:   map[string]string{"kind": "heartbeat"},
	}})
	if len(outcomes) != 1 {
		return fmt.Errorf("the upload port answered %d outcomes for one heartbeat", len(outcomes))
	}
	if outcomes[0] == nil && r.lastCrash != nil && r.OnCrashShipped != nil {
		r.OnCrashShipped()
		r.OnCrashShipped = nil
	}
	return outcomes[0]
}

// ignoreFilter is the .notrajectories attributor: built from the catalog alone, because
// which repositories are tracked is answered by marker files in the repositories, not by
// any config layer.
func ignoreFilter() *sources.RepoFilter {
	compiled, err := sources.Load()
	if err != nil {
		return nil
	}
	return compiled.RepoFilter()
}

// planFor is the one place configuration becomes something the loop can read: the core gets
// values, never the resolver, so adding a config key does not touch the engine.
func planFor(eff *config.Effective) engine.Plan {
	return engine.Plan{
		OrganizationID: eff.OrganizationID,
		StateDir:       eff.StateDir,
		MaxFilesPerRun: eff.MaxFilesPerRun,
		Sources:        eff.Sources,
		RulePacks:      eff.RulePacks,
		SecretKeyNames: eff.SecretKeyNames,
		StructuralEx:   eff.StructuralEx,
		Deny:           eff.Deny,
		Ignore:         ignoreFilter(),
		ConfigVersion:  eff.ConfigVersion,
		ConfigExpired:  eff.ConfigExpired,
	}
}

// Accessors rather than exported fields: a verb reads the runtime, it never swaps a part of it out.

// Effective is the configuration in force.
func (r *Runtime) Effective() *config.Effective { return r.eff }

// Remote is the outcome of this run's config refresh, so a verb can report a fallback.
func (r *Runtime) Remote() controlplane.Remote { return r.remote }

// Destination describes where objects go. A description, never a URL: a ticket's path and query
// are credentials that must not reach a printed line.
func (r *Runtime) Destination() string { return DescribeDestination(r.eff) }

// StateDir is the directory in force, which is not necessarily the default one.
func (r *Runtime) StateDir() string { return r.eff.StateDir }

// AuditLog is the append-only record of what this install decided. Exposed so the scheduler loop
// can record a panic: a run that died leaves no report.
func (r *Runtime) AuditLog() *auditlog.Log { return r.log }

// Recipients are the public keys objects are encrypted to, as strings: public halves only.
func (r *Runtime) Recipients() []string {
	out := make([]string, 0, len(r.recipients))
	for _, rec := range r.recipients {
		if s, ok := rec.(fmt.Stringer); ok {
			out = append(out, s.String())
		}
	}
	return out
}

// FilterSources narrows a run to one source, on a copy: narrowing a view must not mutate the
// configuration the next verb reads.
func (r *Runtime) FilterSources(id string) {
	clone := *r.eff
	clone.Sources = nil
	for _, s := range r.eff.Sources {
		if s.ID == id {
			clone.Sources = append(clone.Sources, s)
		}
	}
	r.eff = &clone
}

// NewBuild describes this binary. The version comes from what the toolchain stamped rather than
// from a flag, so there is exactly one answer and no way for a caller to supply a different one.
func NewBuild() Build {
	return Build{Version: platform.Current().String(), Release: platform.Current().Release}
}

// clientBlock is the build identity stamped into every object. One place, because it is a wire
// contract the ETL groups on: a second construction site is how two objects disagree.
func clientBlock() transforms.Client {
	b := platform.Current()
	return transforms.Client{
		Version:   b.String(),
		Commit:    b.Revision,
		Modified:  b.Modified,
		GoVersion: b.GoVersion,
		OS:        b.OS,
		Arch:      b.Arch,
	}
}
