package cli

import (
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/motleyhand/binpack/internal/controller"
)

func newRunCommand(opts *options) *cobra.Command {
	var (
		path        string
		kubeconfig  string
		kubecontext string

		once                    bool
		leaderElection          bool
		leaderElectionNamespace string
		leaseDuration           time.Duration
		renewDeadline           time.Duration
		retryPeriod             time.Duration
		metricsAddress          string
		probeAddress            string
	)

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run binpack as a controller",
		Long: "Evaluates the cluster on an interval and reports what binpack would do.\n\n" +
			"Defaults to dry run, which decides everything and changes nothing. Each evaluation\n" +
			"reaches the identical decision either way, so running it that way first tells you\n" +
			"which node binpack would pick and why. It cannot tell you what follows: nothing is\n" +
			"drained, so the cluster never consolidates and binpack goes on choosing a first\n" +
			"node — set dryRun: false when you are content with the answers.\n\n" +
			"Acting means four changes and no others: cordon a node, annotate it, uncordon it,\n" +
			"and evict a pod through the eviction API so disruption budgets are respected.\n" +
			"binpack deletes nothing; the cluster-autoscaler removes the emptied node.\n\n" +
			"Decisions surface as Kubernetes Events on the node as well as in the log, because\n" +
			"on a managed control plane `kubectl describe node` is the one place a cluster user\n" +
			"can reliably look — the same way the cluster-autoscaler's own decisions surface.\n\n" +
			"Use --once to evaluate a single time and exit, for running binpack as a CronJob\n" +
			"instead of a Deployment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, source, err := loadConfigOrDefaults(path, cmd.InOrStdin())
			if err != nil {
				return err
			}

			restCfg, err := runRestConfig(kubeconfig, kubecontext)
			if err != nil {
				return err
			}

			// To stderr, so that a `run` piped somewhere is not confused with
			// the machine-readable output of `explain` and `diagnose`.
			// controller-runtime logs through the same logger, so its cache
			// and leader-election messages share the stream and the format.
			log := zap.New(zap.UseDevMode(false), zap.WriteTo(cmd.ErrOrStderr()))
			ctrl.SetLogger(log)

			settings := cfg.Settings()
			log.WithName("binpack").Info("configuration loaded", "from", source)
			return controller.Run(cmd.Context(), controller.Options{
				RestConfig:              restCfg,
				Engine:                  engineConfig(cfg),
				AutoscalerNamespace:     cfg.Discovery.AutoscalerNamespace,
				Log:                     log.WithName("binpack"),
				Interval:                settings.Interval,
				DryRun:                  settings.DryRun,
				Once:                    once,
				LeaderElection:          leaderElection,
				LeaderElectionNamespace: leaderElectionNamespace,
				LeaseDuration:           leaseDuration,
				RenewDeadline:           renewDeadline,
				RetryPeriod:             retryPeriod,
				MetricsAddress:          metricsAddress,
				ProbeAddress:            probeAddress,
			})
		},
	}

	cmd.Flags().StringVarP(&path, "file", "f", "", "configuration file (defaults apply when absent)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig (defaults to in-cluster, then the usual rules)")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().DurationVar(&leaseDuration, "lease-duration", controller.DefaultLeaseDuration,
		"how long a leader election lease is held before another replica may take it")
	cmd.Flags().DurationVar(&renewDeadline, "renew-deadline", controller.DefaultRenewDeadline,
		"how long the leader keeps trying to renew before giving up and exiting")
	cmd.Flags().DurationVar(&retryPeriod, "retry-period", controller.DefaultRetryPeriod,
		"how often the lease is renewed or contested")
	cmd.Flags().BoolVar(&once, "once", false,
		"evaluate once and exit, for running as a CronJob rather than a Deployment")
	// On by default. A single replica still runs two pods during a rolling
	// update, and two binpacks draining at once is precisely the failure the
	// lease exists to prevent.
	cmd.Flags().BoolVar(&leaderElection, "leader-election", true,
		"coordinate through a Lease so only one binpack acts at a time")
	cmd.Flags().StringVar(&leaderElectionNamespace, "leader-election-namespace", "",
		"namespace holding the Lease (defaults to the namespace binpack runs in)")
	cmd.Flags().StringVar(&metricsAddress, "metrics-bind-address", ":8080",
		"address for the Prometheus metrics endpoint, or 0 to disable")
	cmd.Flags().StringVar(&probeAddress, "health-probe-bind-address", ":8081",
		"address for the health and readiness endpoints, or 0 to disable")

	return cmd
}

// runRestConfig resolves a connection for the controller.
//
// Unlike the read-only commands, `run` normally has no kubeconfig at all: it
// runs in a pod with a service account. controller-runtime's resolver tries
// in-cluster credentials first and falls back to the usual kubeconfig rules,
// which is what makes the same binary work in a cluster and on a laptop.
func runRestConfig(kubeconfig, kubecontext string) (*rest.Config, error) {
	if kubeconfig != "" {
		return restConfigFor(kubeconfig, kubecontext)
	}
	return ctrlconfig.GetConfigWithContext(kubecontext)
}
