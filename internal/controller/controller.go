// Package controller runs binpack inside a cluster.
//
// It owns the controller-runtime manager, its caches and leader election, and
// nothing else: the decision is [engine.Decide]'s, and reading the cluster is
// [collect]'s. What lives here is the machinery that makes those two run on a
// schedule, safely, with one binpack acting at a time.
package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/engine"
)

// LeaderElectionID is the Lease binpack coordinates on. Fixed, like the node
// annotations: one thing to document, one thing to look for, and no way for
// two deployments to disagree about which lease means what.
const LeaderElectionID = "binpack.motleyhand.com"

// Options configures a run.
type Options struct {
	RestConfig *rest.Config
	Engine     engine.Config
	Log        logr.Logger

	// Interval is how often the cluster is evaluated.
	Interval time.Duration

	// DryRun decides everything and changes nothing. The default, and the
	// only mode this build implements — the executor is a later step.
	DryRun bool

	// Once evaluates a single time and exits, for running binpack as a
	// CronJob rather than a Deployment. It skips leader election and the
	// metrics and probe servers, none of which mean anything to a process
	// that is about to exit.
	Once bool

	LeaderElection          bool
	LeaderElectionNamespace string
	MetricsAddress          string
	ProbeAddress            string
}

// Run starts binpack and blocks until ctx is cancelled, or until one
// evaluation completes when Options.Once is set.
func Run(ctx context.Context, opts Options) error {
	// Refused rather than quietly degraded. Someone who sets dryRun: false has
	// decided binpack should act; running anyway and only logging "would
	// drain" would leave them believing it is acting for as long as it takes
	// them to check a node.
	if !opts.DryRun {
		return fmt.Errorf(
			"dryRun is false, but this build cannot act on a decision: the executor is not " +
				"implemented yet. Set dryRun: true to run binpack as a reporter")
	}

	mgr, err := manager.New(opts.RestConfig, managerOptions(opts))
	if err != nil {
		return fmt.Errorf("creating the manager: %w", err)
	}

	if !opts.Once {
		// Liveness and readiness are the same ping here. binpack holds no
		// long-lived connection of its own and does nothing between ticks, so
		// there is no state a deeper check could report on — and a probe that
		// invented one would fail for reasons unrelated to whether the process
		// is working.
		if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("adding the health check: %w", err)
		}
		if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
			return fmt.Errorf("adding the readiness check: %w", err)
		}
	}

	// Cancelled by the evaluator itself in Once mode, which is how a single
	// pass stops a manager that would otherwise run forever.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	ev := &evaluator{
		reader:   mgr.GetClient(),
		reporter: reporterFor(opts, mgr),
		opts:     opts,
		log:      opts.Log,
		stop:     stop,
	}
	if err := mgr.Add(ev); err != nil {
		return fmt.Errorf("adding the evaluator: %w", err)
	}

	// Sequenced rather than nested: ev.err is only set once Start has
	// returned, and Go does not define the evaluation order of a plain field
	// read against a call in the same argument list.
	startErr := mgr.Start(runCtx)
	return outcome(startErr, ev.err)
}

// outcome combines what the manager reported with what a one-shot evaluation
// recorded.
//
// The second is not redundant, which is the whole reason this has a name. A
// runnable's error is discarded once the manager's stop sequence is engaged,
// and engaging it is exactly what a one-shot run does, so without this a
// binpack that failed exits 0. See [evaluator.Start].
func outcome(managerErr, evaluationErr error) error {
	if managerErr != nil {
		return fmt.Errorf("running: %w", managerErr)
	}
	return evaluationErr
}

func managerOptions(opts Options) manager.Options {
	out := manager.Options{
		Logger: opts.Log,
		Cache:  cacheOptions(),
		// Nothing here serves traffic or answers webhooks, so leaving these
		// off in Once mode is not a reduced mode of operation — it is the
		// absence of servers a process about to exit would never be scraped
		// or probed on.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	}

	if opts.Once {
		return out
	}

	out.Metrics = metricsserver.Options{BindAddress: opts.MetricsAddress}
	out.HealthProbeBindAddress = opts.ProbeAddress
	out.LeaderElection = opts.LeaderElection
	out.LeaderElectionID = LeaderElectionID
	out.LeaderElectionNamespace = opts.LeaderElectionNamespace
	// Released on shutdown so a rolling update hands over in seconds rather
	// than waiting out the lease. The failure this guards against — two
	// binpacks draining at once — is worst exactly during a deploy, when both
	// the old and new pods are alive.
	out.LeaderElectionReleaseOnCancel = true

	return out
}

// cacheOptions keeps the watch-backed cache to what binpack actually reads.
//
// The ConfigMap restriction is the one that matters. binpack reads exactly one
// ConfigMap — the cluster-autoscaler's status — and an unrestricted cache
// would watch and hold *every* ConfigMap in the cluster to serve that one Get.
// On a busy cluster that is hundreds of megabytes of Helm release data and
// certificate bundles, held permanently, by a tool whose entire purpose is to
// reduce what the cluster costs.
func cacheOptions() cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.ConfigMap{}: {
				Namespaces: map[string]cache.Config{
					collect.StatusConfigMapNamespace: {
						FieldSelector: fields.OneTermEqualSelector(
							"metadata.name", collect.StatusConfigMapName),
					},
				},
			},
			// Nodes and pods are read in full — a pod in any namespace
			// occupies its node — but managed fields are not read at all, and
			// on a large cluster they are a substantial fraction of a pod's
			// stored size.
			&corev1.Node{}:                  {Transform: dropManagedFields},
			&corev1.Pod{}:                   {Transform: dropManagedFields},
			&policyv1.PodDisruptionBudget{}: {Transform: dropManagedFields},
		},
	}
}

// dropManagedFields strips server-side-apply bookkeeping before an object is
// cached. binpack never reads it, and never writes with server-side apply, so
// the only thing it costs here is memory.
func dropManagedFields(in any) (any, error) {
	if obj, ok := in.(client.Object); ok {
		obj.SetManagedFields(nil)
	}
	return in, nil
}

// reporterFor picks how decisions reach the node, which depends on how long
// this process is going to live. See [reporter].
func reporterFor(opts Options, mgr manager.Manager) reporter {
	// The recorder is built lazily: asking a manager for one starts a
	// broadcaster, and a Once run has no use for it.
	var recorder events.EventRecorder
	if !opts.Once {
		recorder = mgr.GetEventRecorder(reportingController)
	}
	return reporterForClient(opts, mgr.GetClient(), recorder)
}

// reporterForClient is the choice itself, separated from the manager so it can
// be exercised without starting one.
func reporterForClient(opts Options, writer client.Writer, recorder events.EventRecorder) reporter {
	if opts.Once {
		return directReporter{
			writer:   writer,
			instance: reportingInstance(),
			now:      time.Now,
		}
	}
	return broadcastReporter{recorder: recorder}
}

// evaluator is the periodic decision loop.
type evaluator struct {
	reader   collect.Reader
	reporter reporter
	opts     Options
	log      logr.Logger
	stop     context.CancelFunc

	// err carries a one-shot run's outcome back to [Run]. See Start.
	err error
}

// NeedLeaderElection puts the evaluator behind the lease. Everything binpack
// does that matters happens here, so this is the whole reason leader election
// exists in this process.
func (e *evaluator) NeedLeaderElection() bool { return true }

// Start satisfies manager.Runnable. The manager runs it once the caches have
// synced and, when enabled, once this process holds the lease.
func (e *evaluator) Start(ctx context.Context) error {
	if e.opts.Once {
		// Recorded rather than returned, which looks redundant and is not.
		// The manager discards a runnable's error once its stop sequence is
		// engaged — and engaging it is exactly what a one-shot run does — so
		// an error returned here is logged as "received after stop sequence
		// was engaged" and thrown away, and the process exits 0 having
		// failed. Run reads this after Start.
		e.err = e.evaluate(ctx)
		e.stop()
		return nil
	}

	e.log.Info("binpack is running",
		"interval", e.opts.Interval, "dryRun", e.opts.DryRun)

	// Evaluated immediately rather than after one interval. A controller that
	// says nothing for its first minute is indistinguishable from one that is
	// broken, and that is the minute somebody is watching.
	if err := e.evaluate(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(e.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := e.evaluate(ctx); err != nil {
				return err
			}
		}
	}
}

// evaluate reads the cluster once and reports what binpack would do.
func (e *evaluator) evaluate(ctx context.Context) error {
	snapshot, err := collect.Snapshot(ctx, e.reader, time.Now())
	if err != nil {
		// Returned rather than logged and swallowed. A read that fails every
		// tick is a broken deployment — bad RBAC, most likely — and a
		// controller that hides it behind a log line looks healthy while doing
		// nothing at all.
		return fmt.Errorf("reading the cluster: %w", err)
	}

	// Before deciding, not at startup. A configuration naming a pool that is
	// not there must not be accepted by `run` while `explain` refuses it —
	// and of the three commands this is the one that will eventually act on
	// it, so silently applying the default policy to a pool an operator
	// believes they switched off is the failure that costs something.
	if err := engine.CheckPools(snapshot, e.opts.Engine); err != nil {
		return err
	}

	decision := engine.Decide(snapshot, e.opts.Engine)

	if err := e.report(ctx, snapshot, decision); err != nil {
		if e.opts.Once {
			// Nothing will retry: this process is about to exit, and its logs
			// go with it. A CronJob that exits 0 having reported nothing is
			// indistinguishable from one that found nothing to report.
			return err
		}
		// The next tick re-decides and re-reports, and the recorder will
		// aggregate it into the same event. Worth knowing about, not worth
		// stopping for — on a cluster where events are the only reachable
		// surface, a crash loop would take the logs away too.
		e.log.Error(err, "could not record the decision on the node")
	}
	return nil
}

var _ manager.LeaderElectionRunnable = (*evaluator)(nil)
