// Package controller runs binpack inside a cluster.
//
// It owns the controller-runtime manager, its caches and leader election, and
// nothing else: the decision is [engine.Decide]'s, and reading the cluster is
// [collect]'s. What lives here is the machinery that makes those two run on a
// schedule, safely, with one binpack acting at a time.
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/tools/leaderelection"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/executor"
	"github.com/motleyhand/binpack/internal/metrics"
)

// LeaderElectionID is the Lease binpack coordinates on. Fixed, like the node
// annotations: one thing to document, one thing to look for, and no way for
// two deployments to disagree about which lease means what.
const LeaderElectionID = "binpack.motleyhand.com"

// Lease timings, and why they are not controller-runtime's defaults.
//
// Those defaults — 15s/10s/2s — suit a controller that reconciles constantly,
// where seconds of lost leadership are seconds of unhandled events. binpack
// evaluates once a minute and keeps a drain's state on the node it is
// draining, so a slow handover costs nothing: the next leader reads the same
// markers and carries on.
//
// What a restart *does* cost is the little binpack holds in memory. The
// after-drain cooldown is measured from a completed drain this process
// remembers, and the completion of a drain whose node has already gone is
// counted from the same memory. Both were justified on restarts being rare —
// and a two-replica-second API-server hiccup restarting the process makes them
// less rare than that argument assumed. It is also correlated in the worst
// way: the control plane is slowest exactly when the cluster is churning,
// which is when binpack is most likely to be mid-drain.
//
// So the lease is sized to ride out a control-plane hiccup instead. The cost
// is failover latency — a leader that genuinely dies leaves the next one
// waiting up to LeaseDuration — which for a controller that does nothing for
// a minute at a time is not a cost at all.
const (
	DefaultLeaseDuration = 60 * time.Second
	DefaultRenewDeadline = 40 * time.Second
	DefaultRetryPeriod   = 10 * time.Second
)

// DefaultGracefulShutdown is how long the manager waits for the evaluator to
// return before giving up on it, once a signal has cancelled its context.
//
// Set rather than left to controller-runtime's own default, because the chart's
// terminationGracePeriodSeconds has to be longer than it — the lease is released
// after the runnables have stopped, so a pod killed inside this window is killed
// at the moment of the handover — and a number binpack does not name is one the
// chart could only guess at. That is exactly the shape of guess this codebase
// keeps getting wrong about other people's defaults.
//
// Short, because the evaluator returns as soon as its context is cancelled and
// the calls it has in flight fail with it. This is a bound on an API call that
// hangs, not a wait binpack expects to use.
const DefaultGracefulShutdown = 15 * time.Second

// maxConsecutiveFailures is how many evaluations may fail in a row before
// binpack stops rather than going on retrying.
//
// The bound is the other half of not exiting on a failed write. Swallowing one
// is right for a bad minute and wrong for a deployment that will never work
// again — a narrowed nodes/patch grant, an admission webhook that denies
// binpack's patches — where a quiet retry every interval leaves a controller
// reporting healthy while doing nothing at all. That reading is exactly what
// returning the error used to protect against, and it is worth keeping.
//
// Five, which at the default interval is five minutes: long enough to ride out
// a control-plane upgrade or a disruption budget that is only briefly
// exhausted, short enough that something permanent shows up as a restarting
// pod. The error counter moves from the first failure either way, so a cluster
// with monitoring sees it immediately and does not depend on this number.
const maxConsecutiveFailures = 5

// unretryable marks a failure that will still be there on the next tick, so it
// leaves the evaluator rather than being counted and retried.
//
// One member today: a configuration naming a pool that is not in the cluster.
// Waiting does not fix a typo, and carrying on would silently apply the
// default policy to a pool an operator believes they switched off.
type unretryable struct{ error }

// Options configures a run.
type Options struct {
	RestConfig *rest.Config
	Engine     engine.Config
	Log        logr.Logger

	// Interval is how often the cluster is evaluated.
	Interval time.Duration

	// DryRun decides everything and changes nothing: no cordon, no eviction,
	// no annotation. The default, and the mode to run first — each evaluation
	// reaches the identical decision either way, so the events on nodes say
	// which node binpack would pick and why before it picks one.
	//
	// What they cannot say is what follows, because in this mode nothing is
	// drained: the cluster never consolidates, so binpack goes on choosing a
	// first node, and every control that governs a second one — the after-drain
	// cooldown, per-node backoff, the one-drain-at-a-time gate — is unreachable.
	DryRun bool

	// Once evaluates a single time and exits, for running binpack as a
	// CronJob rather than a Deployment. It skips leader election and the
	// metrics and probe servers, none of which mean anything to a process
	// that is about to exit.
	Once bool

	LeaderElection          bool
	LeaderElectionNamespace string

	// LeaseDuration, RenewDeadline and RetryPeriod tune the lease. Zero means
	// binpack's own defaults above, not controller-runtime's.
	LeaseDuration  time.Duration
	RenewDeadline  time.Duration
	RetryPeriod    time.Duration
	MetricsAddress string
	ProbeAddress   string
}

// Run starts binpack and blocks until ctx is cancelled, or until one
// evaluation completes when Options.Once is set.
// cooldownIsUnenforceable reports the pool whose after-drain cooldown a
// one-shot run could not honour, if there is one.
//
// A drain outlives many evaluations, and in --once mode each of those is a
// separate process. The completion timestamp lives in memory — a successful
// drain deletes the node that would otherwise have recorded it — so a CronJob
// starts every invocation having forgotten that it ever drained anything, and
// the cooldown never applies.
//
// Refused rather than quietly degraded, and refused only when it would matter:
// a dry run changes nothing to need settling after.
//
// Which pool set it is the engine's question, not this one's: `binpack
// explain` is in exactly the same position — a process that did not perform
// the drain — and asks it too.
func cooldownIsUnenforceable(opts Options) (string, time.Duration, bool) {
	if !opts.Once || opts.DryRun {
		return "", 0, false
	}
	return engine.CooldownAfterDrain(opts.Engine)
}

func Run(ctx context.Context, opts Options) error {
	// Refused rather than left to fail deeper: client-go rejects these
	// orderings too, but from inside the manager, with a message about a
	// leader elector rather than about the flags somebody set.
	if err := checkLease(opts); err != nil {
		return err
	}

	// Before anything else, because the alternative is a safety control that
	// is configured, reported as configured, and silently inert.
	if where, d, unenforceable := cooldownIsUnenforceable(opts); unenforceable {
		return fmt.Errorf(
			"--once cannot honour cooldown.afterDrain (%s sets %s): each run is a new process, "+
				"and %s, so the cooldown would never apply. Run binpack as a Deployment, or set "+
				"cooldown.afterDrain: 0 to say that consecutive drains are acceptable",
			where, d, engine.NoDrainToMeasureFrom)
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
		writer:   mgr.GetClient(),
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

	// Not tunable, unlike the lease timings beside it: the chart's
	// terminationGracePeriodSeconds is checked against this constant, and a
	// flag would let the pod's deadline and the shutdown it is waiting for
	// disagree without anything noticing.
	graceful := DefaultGracefulShutdown
	out.GracefulShutdownTimeout = &graceful

	out.LeaseDuration = orDefault(opts.LeaseDuration, DefaultLeaseDuration)
	out.RenewDeadline = orDefault(opts.RenewDeadline, DefaultRenewDeadline)
	out.RetryPeriod = orDefault(opts.RetryPeriod, DefaultRetryPeriod)

	return out
}

// checkLease refuses timings that cannot work.
//
// A renew deadline at or past the lease duration means the leader can still
// believe it holds a lease another replica has already taken — two binpacks
// draining at once, which is the one thing the lease exists to prevent. A
// retry period at or past the renew deadline leaves no room to retry at all.
func checkLease(opts Options) error {
	// Only when the lease is actually used. A one-shot run skips leader
	// election entirely, and refusing it over timings it will never apply
	// would be a spurious failure in the mode with no operator watching.
	if opts.Once || !opts.LeaderElection {
		return nil
	}

	lease := orDefault(opts.LeaseDuration, DefaultLeaseDuration)
	renew := orDefault(opts.RenewDeadline, DefaultRenewDeadline)
	retry := orDefault(opts.RetryPeriod, DefaultRetryPeriod)

	// The jitter factor is client-go's own constant rather than a copy of its
	// value: a copy would be a second place for it to be wrong, and the whole
	// point of checking here is to agree with what will be checked later.
	floor := time.Duration(leaderelection.JitterFactor * float64(*retry))

	switch {
	case *renew >= *lease:
		return fmt.Errorf(
			"renew deadline (%s) must be shorter than the lease duration (%s), or a leader "+
				"can keep acting on a lease another replica has already taken", *renew, *lease)
	case *renew <= floor:
		// Retries are jittered, so the last one before the deadline can land
		// up to JitterFactor late. A renew deadline that only just exceeds the
		// unjittered retry period leaves no room for that, and client-go
		// refuses it — this says so in terms of the flags rather than letting
		// the manager fail with "renewDeadline must be greater than
		// retryPeriod*JitterFactor".
		return fmt.Errorf(
			"renew deadline (%s) must be longer than the retry period (%s) plus its jitter "+
				"(%s), or there is no room to retry a renewal before giving up",
			*renew, *retry, floor)
	}
	return nil
}

func orDefault(d, fallback time.Duration) *time.Duration {
	if d <= 0 {
		d = fallback
	}
	return &d
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

			// Controllers are read for one field: the pod template their
			// replacements are built from. Everything else is dropped before
			// caching — status, and on a ReplicaSet the retained revision
			// history that makes them numerous in the first place.
			&appsv1.ReplicaSet{}:  {Transform: keepTemplateOnly},
			&appsv1.StatefulSet{}: {Transform: keepTemplateOnly},
			&appsv1.DaemonSet{}:   {Transform: keepTemplateOnly},
			&batchv1.Job{}:        {Transform: keepTemplateOnly},
		},
	}
}

// keepTemplateOnly strips a controller down to what binpack reads: its
// identity and its pod template.
//
// A cluster keeps ten ReplicaSet revisions per Deployment by default, and
// binpack looks at exactly one field of each. Holding their full status and
// annotations — which include the last-applied-configuration of the whole
// workload — would make the cache grow with revision history for no benefit.
func keepTemplateOnly(in any) (any, error) {
	switch obj := in.(type) {
	case *appsv1.ReplicaSet:
		obj.Status = appsv1.ReplicaSetStatus{}
		obj.Annotations = nil
	case *appsv1.StatefulSet:
		obj.Status = appsv1.StatefulSetStatus{}
		obj.Annotations = nil
	case *appsv1.DaemonSet:
		obj.Status = appsv1.DaemonSetStatus{}
		obj.Annotations = nil
	case *batchv1.Job:
		obj.Status = batchv1.JobStatus{}
		obj.Annotations = nil
	}
	return dropManagedFields(in)
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
	writer   executor.Writer
	reporter reporter
	opts     Options
	log      logr.Logger
	stop     context.CancelFunc

	// active is the node this process last saw a drain on, kept so the drain
	// can be counted as complete once the node and its markers are gone. See
	// drainInProgress.
	active string

	// lastDrain is when this process last completed one, which is what
	// cooldown.afterDrain measures from.
	//
	// In memory rather than on an object, because a successful drain is
	// exactly the case where there is no object left to write it on — the node
	// it happened to has been deleted. A restart therefore forgets it, and
	// [engine.Snapshot.LastDrain] already says what that costs: the cooldown
	// does not apply, and the worst case is one drain sooner than intended.
	// Persisting it would mean a new API object, a new permission and a new
	// failure mode, to protect against a restart that happens to fall inside
	// one cooldown window.
	lastDrain time.Time

	// consecutiveFailures counts evaluations that failed back to back, and is
	// reset by any that does not. See [evaluator.failed].
	consecutiveFailures int

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

// evaluate runs one pass and decides whether its failure, if it failed, is the
// process's failure too.
//
// Almost never, and it used to be always. Every write binpack makes — a cordon,
// an annotation, an eviction the API server answered 429 because a disruption
// budget's allowance had been drawn on since the snapshot — returned its error
// from here, which returned it from [evaluator.Start], which stops the manager
// and exits the process. On a managed control plane those failures are ordinary
// rather than exceptional, and they cluster during a control-plane upgrade,
// which is when binpack is likeliest to be mid-drain. The 429 in particular is
// documented in the executor as retryable and was the one nothing retried.
//
// Nothing is lost by carrying on. A drain's recovery state is on the node it is
// draining precisely so that the next evaluation can pick it up (ADR-0007), so
// re-reading and re-deciding a minute later is both cheaper and safer than a
// restart — and it is already what a failed report does two functions below.
func (e *evaluator) evaluate(ctx context.Context) error {
	if err := e.attempt(ctx); err != nil {
		return e.failed(ctx, err)
	}
	e.consecutiveFailures = 0
	return nil
}

// failed records an evaluation that could not be completed.
func (e *evaluator) failed(ctx context.Context, err error) error {
	// Shutting down is not a failure. Cancelling the manager's context cancels
	// every call an evaluation has left, so without this a SIGTERM landing
	// mid-drain would move the counter that exists to report a broken cluster
	// and spend one of the retries meant for one — on every rolling update.
	if shuttingDown(ctx) {
		e.log.Info("shutting down; this evaluation was interrupted", "reason", err.Error())
		return nil
	}

	metrics.Failed()

	var permanent unretryable
	switch {
	case errors.As(err, &permanent):
		// Unwrapped, so what reaches the operator is the sentence the check
		// wrote rather than a marker they have no way to interpret.
		return permanent.error

	case e.opts.Once:
		// Nothing will retry: this process is about to exit, and a CronJob
		// that exits 0 having done nothing is indistinguishable from one that
		// found nothing to do. The report path draws the same line.
		return err
	}

	e.consecutiveFailures++
	if e.consecutiveFailures >= maxConsecutiveFailures {
		return fmt.Errorf("%d evaluations in a row have failed, the last with: %w",
			e.consecutiveFailures, err)
	}
	e.log.Error(err, "evaluation failed; the next one re-reads the cluster and re-decides",
		"consecutiveFailures", e.consecutiveFailures)
	return nil
}

// shuttingDown reports whether the manager has begun stopping.
//
// A named question rather than the cancellation read inline, because the two
// readings of a cancelled context are opposites here: every call an evaluation
// makes after this point fails, and none of those failures is a fact about the
// cluster.
func shuttingDown(ctx context.Context) bool { return ctx.Err() != nil }

// attempt reads the cluster once and reports what binpack would do.
func (e *evaluator) attempt(ctx context.Context) error {
	started := time.Now()

	snapshot, err := collect.Snapshot(ctx, e.reader, started)
	if err != nil {
		// Counted rather than returned on sight. A read that fails every tick
		// is a broken deployment — bad RBAC, most likely — and a controller
		// that hides it behind a log line looks healthy while doing nothing at
		// all; but a single failed list is an API server having a moment, and
		// exiting over one made the process's restart backoff, rather than
		// interval, the clock every drain ran on.
		return fmt.Errorf("reading the cluster: %w", err)
	}

	// Before deciding, not at startup. A configuration naming a pool that is
	// not there must not be accepted by `run` while `explain` refuses it —
	// and of the three commands this is the one that will eventually act on
	// it, so silently applying the default policy to a pool an operator
	// believes they switched off is the failure that costs something.
	if err := engine.CheckPools(snapshot, e.opts.Engine); err != nil {
		return unretryable{err}
	}

	// Step 0 of the drain protocol: resume before deciding.
	//
	// A drain can legitimately outlast many intervals, and without this every
	// one of those intervals would be free to select a second node. "One node
	// per run" quietly assumed a run was short.
	if node := e.drainInProgress(snapshot); node != "" {
		if !e.opts.DryRun {
			metrics.Observe(snapshot, engine.Decision{Code: engine.CodeDraining,
				Reason: "a drain is in progress on " + node}, e.opts.Engine,
				time.Since(started).Seconds())
			return e.advance(ctx, snapshot, node)
		}
		// Said, and then carried on past — because in dry run this drain never
		// moves. Step 0 pre-empts a new decision so that a drain outlasting
		// many intervals cannot have a second node cordoned underneath it, and
		// that pre-emption is bounded by the drain ending. Dry run removes the
		// bound: the marker is never cleared, so returning here made binpack
		// silent about every other node in the cluster for as long as the
		// setting stood — no decision, no event, and binpack_nodes publishing
		// an empty set — which is the whole of what dry run is for.
		//
		// Nothing is written on the way through. Deciding costs a simulation
		// and the gate below stops before the executor, as it does on every
		// other dry-run pass.
		if err := e.frozen(ctx, snapshot, node); err != nil {
			if e.opts.Once {
				// Nothing will retry, and unlike the decision below there is
				// no second chance within this run either: a frozen drain
				// writes nothing and advances nothing, so that event is the
				// whole of what the evaluation had to say. Where the decision
				// that follows finds nothing to drain it writes nothing at
				// all, and the run would exit 0 having reported none of it.
				return err
			}
			// The next tick re-reports, and the recorder aggregates it into
			// the same event. Same call as the decision's, for the same
			// reason: on a cluster where events are the only reachable
			// surface, a crash loop takes the logs away too.
			e.log.Error(err, "could not record what would happen to the drain", "node", node)
		}
	}

	// Filled in by the controller rather than read from the cluster: it is the
	// one thing in a snapshot that is not observable there, because a
	// completed drain deletes the node that would have recorded it.
	snapshot.LastDrain = e.lastDrain

	decision := engine.Decide(snapshot, e.opts.Engine)
	metrics.Observe(snapshot, decision, e.opts.Engine, time.Since(started).Seconds())

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

	if decision.Action != engine.Drain || e.opts.DryRun {
		return nil
	}

	// Marked and cordoned, and nothing evicted. Until the node is
	// unschedulable its pod set can still grow, so what leaves it is the next
	// evaluation's decision, against the post-cordon set.
	e.log.Info("draining node", "node", decision.Node.Name)
	if err := executor.Begin(ctx, e.writer, decision.Node, snapshot.Now); err != nil {
		return fmt.Errorf("starting the drain of %s: %w", decision.Node.Name, err)
	}
	metrics.DrainStarted()
	e.active = decision.Node.Name
	return nil
}

// drainInProgress names the node binpack is part-way through draining.
//
// The marker is read by [engine.Marked], which is also what makes
// [engine.Decide] decline to choose a second node — so this short-circuit and
// the decision function cannot disagree about whether a drain is running.
//
// What is added here is the half the engine cannot see: a drain whose node has
// gone. It remembers the last one, because a successful drain is exactly the
// case where the evidence disappears — the autoscaler removes the node and the
// markers go with it. Without this the completion is unobservable, and the
// completed-drain counter would only ever increment for drains that failed to
// finish, which is the opposite of useful.
//
// Process memory rather than an annotation, and that is fine here precisely
// because it is only a metric. Everything a *recovery* needs still lives on
// the nodes; the worst a restart costs is one uncounted drain.
func (e *evaluator) drainInProgress(s engine.Snapshot) string {
	if name := engine.Marked(s); name != "" {
		e.active = name
		return name
	}

	if e.active != "" && !present(s, e.active) {
		// Gone, so [executor.Advance] will report the removal and write
		// nothing. Left to it rather than counted here, so there is one place
		// that decides what the end of a drain means.
		return e.active
	}

	// Either nothing was running, or the node is still there with its marker
	// cleared by something other than binpack. The second is no longer
	// binpack's drain, and acting on it would be acting on a node nobody
	// marked.
	e.active = ""
	return ""
}

func present(s engine.Snapshot, name string) bool {
	for _, node := range s.Nodes {
		if node.Name == name {
			return true
		}
	}
	return false
}

// frozen reports the drain binpack is not advancing, because dryRun is set.
//
// Reachable, and reached by the commonest routes there are: dry run is the
// default and the mode operators are told to return to when unsure, so a helm
// rollback, a reverted values file or an override lost in a chart upgrade all
// arrive here with a drain in flight. The node is cordoned and binpack has been
// told to change nothing, which leaves saying so as the only honest option —
// the alternative is uncordoning, and that is a change.
//
// Saying it on the node rather than only in the log, because this is the one
// state a drain can be left in indefinitely. Every other ending is bounded by
// the assessment; this one is bounded by an operator noticing, and on a managed
// control plane `kubectl describe node` is where they will be looking.
//
// Which is also why losing that event is worth reporting rather than logging.
// Nothing here writes or advances anything, so the event is the whole output of
// the evaluation — and in a one-shot run it is the only durable record there
// is, since the logs go with the process.
func (e *evaluator) frozen(ctx context.Context, s engine.Snapshot, name string) error {
	a := engine.Revalidate(s, name, e.opts.Engine)
	if a.SkipCode == engine.SkipGone {
		// The autoscaler finished a drain that started before dry run was
		// switched on. Nothing is stranded, and forgetting the node is what
		// stops every later evaluation reporting on one that is not there.
		e.log.Info("the node a drain was in progress on has gone", "node", name)
		e.active = ""
		return nil
	}

	would := drain.WouldHappen(a, drain.Assess(
		drain.State{Node: a.Node, Pods: drain.PodsOn(s, name), Now: s.Now}, drain.PolicyFor(e.opts.Engine, s, name)))

	e.log.Info("a drain is in progress but dryRun is set; leaving it alone",
		"node", name, "wouldHappen", would)

	// Returned rather than logged here, like the decision's own report and for
	// the same reason: whether a lost event is fatal depends on whether
	// anything will try again, which the caller knows and this does not.
	if err := e.reporter.emit(ctx, a.Node, ReasonWouldAdvanceDrain, ActionConsolidate,
		"binpack is in dry run, so it is not advancing the drain in progress on this node: "+
			"it stays cordoned and marked until dryRun is set to false. "+would); err != nil {
		return fmt.Errorf("recording what would happen to the drain of %s: %w", name, err)
	}
	return nil
}

// advance moves an in-progress drain on by one step. Only ever reached when
// binpack is acting; a dry run reports through [evaluator.frozen] instead.
func (e *evaluator) advance(ctx context.Context, s engine.Snapshot, name string) error {
	step, err := executor.Advance(ctx, e.writer, s, name, e.opts.Engine, drain.PolicyFor(e.opts.Engine, s, name))
	if err != nil {
		return fmt.Errorf("advancing the drain of %s: %w", name, err)
	}

	if step.Done {
		e.active = ""
	}

	switch {
	case step.Failed:
		metrics.DrainAbandoned(step.Code)
		e.log.Info("drain abandoned", "node", name, "code", step.Code, "reason", step.Reason)
		e.emitDrainEnded(ctx, name, ReasonDrainAbandoned,
			fmt.Sprintf("binpack stopped draining this node: %s. It has been uncordoned",
				step.Reason))
	case step.Done:
		// Only a drain that finished. An abandoned one already records
		// per-node backoff, and letting it start a cluster-wide cooldown as
		// well would punish every other node for one node's failure.
		e.lastDrain = s.Now
		metrics.DrainCompleted()
		e.log.Info("drain complete", "node", name, "reason", step.Reason)
		e.emitDrainEnded(ctx, name, ReasonDrained,
			"binpack finished draining this node and the cluster-autoscaler removed it")
	default:
		// Intermediate steps stay in the log. One eviction per evaluation
		// would otherwise put an event on the node for every pod, and the
		// events worth reading are how the drain started and how it ended.
		e.log.Info("drain advanced", "node", name, "step", step.Code, "reason", step.Reason)
	}
	return nil
}

// emitDrainEnded records how a drain finished on the node it happened to.
//
// Built from the name rather than taken from the snapshot, because the
// successful case is exactly the one where the node is already gone — and that
// event is still worth writing. It outlives the object, which is the point: it
// is the record that the node binpack drained is the node that disappeared.
//
// Logged rather than returned on failure. The drain itself has already
// happened; losing the note about it is not a reason to fail an evaluation
// that will not be repeated.
func (e *evaluator) emitDrainEnded(ctx context.Context, name, reason, note string) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := e.reporter.emit(ctx, node, reason, ActionConsolidate, note); err != nil {
		e.log.Error(err, "could not record how the drain ended", "node", name)
	}
}

var _ manager.LeaderElectionRunnable = (*evaluator)(nil)
