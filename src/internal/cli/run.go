package cli

import (
	"context"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/config"
	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/formats"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/auditlog"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform/crashjournal"
	"github.com/QuesmaOrg/quesma-shipper/packaging"
)

func flushRecovered(ctx context.Context, env *app.Runtime, errOut io.Writer) (
	formats.Report, error, bool,
) {
	return recoverFlush(errOut, env.AuditLog(), func() (formats.Report, error) {
		return env.Flush(ctx, false)
	})
}
func recoverFlush(errOut io.Writer, log *auditlog.Log, flush func() (formats.Report, error)) (
	rep formats.Report, err error, panicked bool,
) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		panicked = true
		stack := string(debug.Stack())
		fmt.Fprintf(errOut, "PANIC in flush: %v\n%s\n", r, stack)
		fmt.Fprintf(errOut, "the tick was abandoned; the next one starts clean. "+
			"whatever caused this is still on disk and will be met again.\n")
		if log != nil {
			_ = log.Append(auditlog.Entry{
				Decision: auditlog.DecisionFailed,
				Reason:   fmt.Sprintf("panic in flush: %v", r),
				File:     firstStackFrame(stack),
			})
		}
		err = fmt.Errorf("panic in flush: %v", r)
	}()
	rep, err = flush()
	return rep, err, false
}
func firstStackFrame(stack string) string {
	for _, line := range strings.Split(stack, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "quesma-shipper/internal/") && strings.Contains(line, ".go:") {
			return line
		}
	}
	return ""
}

const recycleAfter = 3 * time.Hour
const enrollmentPollInterval = 5 * time.Second

func recycleDue(started, now time.Time, serviceLoaded func() bool) bool {
	return now.Sub(started) >= recycleAfter && serviceLoaded()
}

// reportOutcome submits telemetry for whatever was just judged, under a bound of its own so it
// cannot spend the caller's budget: the drain deadline, or the gap before the next tick.
func reportOutcome(ctx context.Context, env *app.Runtime) {
	ctx, cancel := context.WithTimeout(ctx, telemetryDeadline)
	defer cancel()
	env.SubmitTelemetry(ctx)
}

// telemetryDeadline is short on purpose: a report about collection must never be what delays it.
const telemetryDeadline = 5 * time.Second

func flushBeforeExit(cmd *cobra.Command, env *app.Runtime) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "\nsignal received, shipping one final slice before exit")
	ctx, cancel := context.WithTimeout(context.Background(), env.Effective().DrainDeadline)
	defer cancel()
	before := platform.ReadMemStats()
	rep, err := env.Flush(ctx, false)
	// Measured here rather than reused from the tick loop: the facts have to describe THIS slice.
	// The last chance to ship, so its outcome has to outlive the process like a tick's -- including
	// the shape a tick catches, returning nil having failed every upload.
	mem := platform.Delta{Before: before, After: platform.ReadMemStats()}
	tickErr := env.JudgeFinalSlice(err, rep, mem)
	// The last thing this machine does: without it, a final slice that failed every upload is the
	// one outcome an operator never sees.
	reportOutcome(ctx, env)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "final slice failed: %v\n", err)
		return nil
	}
	printRunSummary(out, rep, false)
	if tickErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "final slice sent nothing: %v\n", tickErr)
	}
	if rep.Remaining > 0 {
		fmt.Fprintf(out, "%d files remain; the next start resumes the backlog\n", rep.Remaining)
	}
	return nil
}

func runCmd(build app.Build) *cobra.Command {
	var once, drain, quiet bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the scheduler loop in the foreground",
		Long: "The development and debug mode: logs to stderr, Ctrl-C to stop, zero installation.\n" +
			"A tick is change DETECTION - size and mtime pre-filter, then a content hash - so\n" +
			"only changed sources go on to redact, seal and upload. Missed ticks are harmless:\n" +
			"the backlog is fingerprint-driven and oldest-first, so a late tick is a catch-up.\n" +
			"A tick cut short by max_files_per_run re-ticks after a short pause instead of\n" +
			"waiting the full interval, so a backlog converges at upload speed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := packaging.ValidateRun(); err != nil {
				return err
			}
			if !once && (drain || quiet) {
				return fmt.Errorf("--drain and --quiet require --once")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			// Resolved once, offline: a config that will not parse also reads as "not logged in"
			// and cannot repair itself between polls, so waiting on it waits forever. A resolve
			// error skips the wait and the gates below, and lets app.New report the real reason.
			eff, paths, resolveErr := app.ResolveEffective()
			logStateDir := paths.StateDir
			if logStateDir == "" {
				logStateDir, _ = app.StateDirWithoutConfig()
			}
			if log, err := packaging.ManagedRunLog(logStateDir); err != nil {
				return err
			} else if log != nil {
				defer log.Close()
				cmd.SetOut(log)
				cmd.SetErr(log)
			}
			waiting := false
			var lastManagedAttempt time.Time
			for resolveErr == nil {
				if _, err := controlplane.LoadEnrollment(paths.StateDir); err == nil {
					break
				}
				if time.Since(lastManagedAttempt) >= time.Minute {
					lastManagedAttempt = time.Now()
					attemptCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					enrolled, err := app.ManagedLogin(attemptCtx)
					cancel()
					if enrolled {
						eff, paths, resolveErr = app.ResolveEffective()
						break
					}
					if err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "managed enrollment: %v; retrying in one minute\n", err)
					}
				}
				if !waiting {
					fmt.Fprintln(cmd.ErrOrStderr(), "waiting for enrollment; run `quesma-shipper login` to continue")
					waiting = true
				}
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(enrollmentPollInterval):
				}
				// Login can land in a state_dir edited during the wait: each poll reads the
				// current one, and the gates below see the config as of enrollment.
				eff, paths, resolveErr = app.ResolveEffective()
			}
			if !once {
				maybeSelfUpdate(ctx, build, resolveErr != nil || eff.AutoupdateEnabled, cmd.ErrOrStderr())
			}

			// After the self-update, whose re-exec never returns and would read as a death. NOT
			// deferred: a panic has to unwind past the Exit call, and that missing entry is the
			// crash record. A config too broken to resolve still gets a journal, in the default
			// state directory, the way pause does.
			stateDir, dirErr := paths.StateDir, error(nil)
			if resolveErr != nil {
				stateDir, dirErr = app.StateDirWithoutConfig()
			}
			fl, runID, lastCrash := startCrashJournal(cmd.ErrOrStderr(), stateDir, dirErr)
			err := runLoop(cmd, ctx, build, once, drain, quiet, fl, runID, lastCrash)
			fl.Exit()
			return err
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "collect and send once, then exit")
	cmd.Flags().BoolVar(&drain, "drain", false, "send everything pending and block until done or drain_deadline")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "no progress or summary; warnings, errors and the run log stay")
	return cmd
}

func runLoop(cmd *cobra.Command, ctx context.Context, build app.Build, once, drain, quiet bool,
	fl *crashjournal.Log, runID string, lastCrash *formats.LastCrash) error {
	fl.Phase("init")

	env, err := app.New(build)
	if err != nil {
		// No runtime, so JudgeTick cannot see this: a config that will not resolve used to
		// reach stderr and nothing else, leaving an install broken and silent.
		app.RecordStartupFailure("run", runID, err)
		return err
	}
	env.SetRunInfo(runID, lastCrash)
	env.OnCrashShipped = fl.Reported

	out := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()
	errOut := stderr
	var stream *progressStream
	if once {
		stream = newProgressStream(errOut, quiet)
		env.OnProgress = stream.emit
		env.OnLocked = func() { stream.openLog(env.StateDir()) }
		defer stream.closeLog()
		errOut = stream.Stderr()
	} else {
		packaging.RotateLogs(filepath.Join(env.StateDir(), "logs"))
	}
	reportRemote(errOut, env)

	// Resolved for --once too: the interval doubles as the stall watchdog's threshold.
	tick, tickWarn := config.TickInterval(env.Effective().Schedule)
	printWarning(errOut, tickWarn)
	if !once {
		limit, fromEnv := platform.SetSoftLimit(platform.DefaultSoftLimit)
		source := "default"
		if fromEnv {
			source = "GOMEMLIMIT"
		}
		fmt.Fprintf(out, "memory soft limit %d MB (%s)\n", limit>>20, source)
		fmt.Fprintf(out, "collecting to %s every %s; Ctrl-C to stop\n", env.Destination(), tick)
	}
	started := time.Now()

	for n := 1; ; n++ {
		fl.Phase(fmt.Sprintf("tick %d", n))
		// Roots are picked once, during app.New above. An agent installed -- or first run, which
		// is when Claude Code creates projects/ -- after this process started would otherwise read
		// as absent until something restarted the daemon. Announced, because the silence is what
		// let an install look healthy for days while collecting nothing.
		appeared := env.RefreshAbsentRoots()
		if !quiet {
			for _, id := range appeared {
				fmt.Fprintf(out, "source %s: root resolved, collecting from this tick\n", id)
			}
		}
		before := platform.ReadMemStats()
		var rep formats.Report
		var err error
		var panicked bool
		complete := true
		if drain {
			rep, complete, err = env.Drain(ctx)
		} else {
			// Not armed for a drain, whose legitimate bound is drain_deadline, not the tick interval.
			// Warns on the raw stderr: the progress bar's writer is not safe for a second goroutine.
			wctx, stopWatch := context.WithCancel(context.Background())
			watchdogDone := make(chan struct{})
			go func() {
				env.WatchStalledTick(wctx, n, tick, stderr)
				close(watchdogDone)
			}()
			rep, err, panicked = flushRecovered(ctx, env, errOut)
			// Joined, not just signalled: the judge is about to write the report the watchdog reads.
			stopWatch()
			<-watchdogDone
		}
		mem := platform.Delta{Before: before, After: platform.ReadMemStats()}
		if stream != nil {
			stream.Finish()
		}
		if ctx.Err() != nil {
			if once {
				return err
			}
			return flushBeforeExit(cmd, env)
		}
		// Judged before anything else reports: the record has to outlive the run, and a locally
		// caused failure ships its own failure heartbeat from here.
		tickErr := env.JudgeTick(err, rep, panicked, mem)
		if err == nil && !quiet {
			printRunSummary(out, rep, once && !drain)
			if !once {
				fmt.Fprintf(out, "  memory\t%s\n", mem)
			}
		}
		// After judging, so this tick's outcome is included, and after the summary, so a slow
		// collector cannot hold back the line the operator reads.
		reportOutcome(ctx, env)

		// The flush keeps its own error and exit code -- lock contention still reads as the refusal
		// it always did; the verdict only ADDS the run that completed and sent nothing.
		outcome := err
		if outcome == nil {
			outcome = tickErr
		}
		if outcome != nil {
			if once {
				return outcome
			}
			fmt.Fprintf(errOut, "flush failed: %v\n", outcome)
		}
		if once {
			if drain && !complete {
				return fmt.Errorf("drain hit its %s deadline with work left: raise drain_deadline or accept the loss",
					env.Effective().DrainDeadline)
			}
			if drain && !quiet {
				fmt.Fprintln(out, "drain complete: nothing pending")
			}
			return nil
		}
		if recycleDue(started, time.Now(), func() bool {
			return packaging.ServiceState(env.StateDir()).Loaded
		}) {
			fmt.Fprintf(out, "recycling after %s of uptime; replacing the process in place\n",
				time.Since(started).Round(time.Second))
			clearSelfUpdateHop()
			if err := packaging.ReExec(); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "recycle: re-exec failed (%v); exiting for the supervisor\n", err)
				app.RecordUpdateFailure(fmt.Sprintf("recycle re-exec failed, falling back to the supervisor: %v", err))
			}
			return nil
		}

		select {
		case <-ctx.Done():
			return flushBeforeExit(cmd, env)
		case <-time.After(app.NextDelay(rep, err, tick)):
		}
	}
}
