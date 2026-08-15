package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/motleyhand/binpack/api/v1alpha1"
)

func newConfigCommand(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Work with binpack configuration",
	}
	cmd.AddCommand(newConfigValidateCommand(opts))
	return cmd
}

func newConfigValidateCommand(opts *options) *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check a configuration file and show what it resolves to",
		Long: "Parses a configuration file, applies defaults and validates it, then prints the\n" +
			"result. Reads standard input when no file is given, so it can be piped from\n" +
			"`kubectl get configmap ... -o jsonpath`.\n\n" +
			"Empty input is valid: pools and their bounds are discovered from the\n" +
			"cluster-autoscaler rather than declared, so the defaults are a working\n" +
			"configuration.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			data, err := readConfigInput(path, cmd.InOrStdin())
			if err != nil {
				return err
			}
			return validateConfig(opts, data)
		},
	}

	cmd.Flags().StringVarP(&path, "file", "f", "", "configuration file to read (default: stdin)")

	return cmd
}

func readConfigInput(path string, stdin io.Reader) ([]byte, error) {
	if path == "" {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("reading standard input: %w", err)
		}
		return data, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

// resolvedConfig is what both output formats report: the effective settings,
// not the sparse document that produced them. Echoing the input back would
// tell an operator nothing they did not already type.
type resolvedConfig struct {
	Interval      string               `json:"interval"`
	DryRun        bool                 `json:"dryRun"`
	Discovery     v1alpha1.Discovery   `json:"discovery"`
	DefaultPolicy policyView           `json:"defaultPolicy"`
	Pools         []resolvedPoolPolicy `json:"pools,omitempty"`
}

type resolvedPoolPolicy struct {
	Name   string     `json:"name"`
	Policy policyView `json:"policy"`
}

// policyView renders a resolved policy for output. Durations become strings
// here rather than in the API type, which keeps time.Duration as the Go-facing
// value and confines presentation to the CLI.
type policyView struct {
	Enabled                  bool     `json:"enabled"`
	ExpendablePriorityCutoff int32    `json:"expendablePriorityCutoff"`
	ReserveForLargestPod     bool     `json:"reserveForLargestPod"`
	MaxPodsPerDrain          int      `json:"maxPodsPerDrain"`
	StallTimeout             string   `json:"stallTimeout"`
	RemovalTimeout           string   `json:"removalTimeout"`
	BackoffInitial           string   `json:"backoffInitial"`
	BackoffMax               string   `json:"backoffMax"`
	CooldownAfterScaleUp     string   `json:"cooldownAfterScaleUp"`
	CooldownAfterDrain       string   `json:"cooldownAfterDrain"`
	ExcludedNamespaces       []string `json:"excludedNamespaces,omitempty"`
}

func viewOf(p v1alpha1.PoolPolicy) policyView {
	return policyView{
		Enabled:                  p.Enabled,
		ExpendablePriorityCutoff: p.ExpendablePriorityCutoff,
		ReserveForLargestPod:     p.ReserveForLargestPod,
		MaxPodsPerDrain:          p.MaxPodsPerDrain,
		StallTimeout:             p.StallTimeout.String(),
		RemovalTimeout:           p.RemovalTimeout.String(),
		BackoffInitial:           p.BackoffInitial.String(),
		BackoffMax:               p.BackoffMax.String(),
		CooldownAfterScaleUp:     p.CooldownAfterScaleUp.String(),
		CooldownAfterDrain:       p.CooldownAfterDrain.String(),
		ExcludedNamespaces:       p.ExcludedNamespaces,
	}
}

func resolve(cfg *v1alpha1.Config) resolvedConfig {
	r := resolvedConfig{
		Interval:      cfg.Interval.String(),
		DryRun:        *cfg.DryRun,
		Discovery:     cfg.Discovery,
		DefaultPolicy: viewOf(cfg.PolicyFor()),
	}
	for _, pool := range cfg.Pools {
		r.Pools = append(r.Pools, resolvedPoolPolicy{
			Name:   pool.Name,
			Policy: viewOf(cfg.PolicyFor(pool.Name)),
		})
	}
	return r
}

func validateConfig(opts *options, data []byte) error {
	cfg, err := v1alpha1.Load(data)
	if err != nil {
		return err
	}

	if opts.output == outputJSON {
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		return enc.Encode(resolve(cfg))
	}

	return writeConfigSummary(opts, cfg)
}

func writeConfigSummary(opts *options, cfg *v1alpha1.Config) error {
	var errs []error
	p := func(format string, args ...any) {
		if _, err := fmt.Fprintf(opts.out, format, args...); err != nil {
			errs = append(errs, err)
		}
	}

	p("configuration is valid\n\n")
	p("interval:  %s\n", cfg.Interval)
	p("dry run:   %t", *cfg.DryRun)
	if *cfg.DryRun {
		p("  (decides everything, changes nothing)")
	}
	p("\n")
	p("pool label: %s\n", cfg.Discovery.PoolNameLabel)
	p("group label: %s\n", cfg.Discovery.NodeGroupIDLabel)

	// The resolved default policy is what most pools will actually get, and
	// is far more useful to see than the sparse document that produced it.
	p("\ndefault policy for every discovered pool:\n")
	writePolicy(p, cfg.PolicyFor())

	for _, pool := range cfg.Pools {
		p("\noverride for pool %q:\n", pool.Name)
		writePolicy(p, cfg.PolicyFor(pool.Name))
	}

	return errors.Join(errs...)
}

func writePolicy(p func(string, ...any), policy v1alpha1.PoolPolicy) {
	p("  enabled:                 %t\n", policy.Enabled)
	p("  expendable below:        priority %d\n", policy.ExpendablePriorityCutoff)
	p("  reserve for largest pod: %t\n", policy.ReserveForLargestPod)
	if policy.MaxPodsPerDrain == 0 {
		p("  max pods per drain:      unlimited\n")
	} else {
		p("  max pods per drain:      %d\n", policy.MaxPodsPerDrain)
	}
	p("  abandon if stalled for:  %s\n", policy.StallTimeout)
	p("  await removal for:       %s\n", policy.RemovalTimeout)
	p("  backoff after failure:   %s, doubling to %s\n", policy.BackoffInitial, policy.BackoffMax)
	p("  cooldown after scale-up: %s\n", policy.CooldownAfterScaleUp)
	p("  cooldown after drain:    %s\n", policy.CooldownAfterDrain)
	if len(policy.ExcludedNamespaces) > 0 {
		p("  excluded namespaces:     %v\n", policy.ExcludedNamespaces)
	}
}
