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

// The two things this command knowingly leaves unverified, in the sentences
// the summary prints for them — the disclosure pattern explain already has
// for controls it cannot evaluate.
//
// Both name something only a cluster can confirm, and both are confirmed in
// the same place: [engine.ResolvePools] refuses an override naming a pool the
// cluster has not got, and a stated join naming a node group the autoscaler
// does not publish. Two sentences rather than one, because they are separate
// conditions — a document can state either without the other, and a single
// disclosure conditioned on the pool overrides said nothing about a document
// carrying only a join. Each is written to stand alone for that reason, so
// neither may lean on the other having been printed.
//
// Neither promises when the refusal arrives, and that is deliberate rather
// than vague. The controller resolves pools on every evaluation but holds the
// error until after it has resumed any drain in progress, because the failure
// is unretryable and exiting one tick into a drain strands a cordoned node —
// so `run --once` can advance a drain and exit before it reports either. What
// is true without qualification is that the check needs a cluster, that this
// command has none, and that an unknown name is fatal to the controller
// whenever it does reach it. Pointing at the read-only command that answers
// the question beats asserting a timing on the operator's behalf.
//
// The summary only, deliberately. `--output json` reports a configuration
// document: apiVersion, kind, and every key a configuration field, so that
// feeding it back through -f is valid and resolves to the same settings. The
// loader rejects unknown fields, so a disclosure key there would make the
// report unloadable for precisely the documents these sentences are about.
// And there is nothing to disclose to that reader: the JSON never says the
// configuration is valid, it says what the document resolves to, which is
// true of an entry whether or not the cluster has what it names.
const (
	poolNamesNotChecked = "pools[].name is not checked here: this command reads a document, " +
		"not a cluster. Run `binpack explain` or `binpack diagnose` against the cluster to " +
		"check each name against the pools it has; the controller treats an unknown one as a " +
		"fatal configuration error."

	nodeGroupJoinsNotChecked = "discovery.nodeGroups is not checked here: a stated join must " +
		"name a node group the cluster-autoscaler publishes, so only a cluster can confirm it. " +
		"Run `binpack explain` or `binpack diagnose`; the controller treats an unknown group " +
		"as a fatal configuration error."
)

// resolvedConfig is what `--output json` reports: the effective settings, not
// the sparse document that produced them. Echoing the input back would tell an
// operator nothing they did not already type.
//
// A configuration document all the same, in the document's own vocabulary,
// with every value that was inherited or defaulted written out explicitly. It
// used to be a parallel one: `defaultPolicy.backoffInitial` where the document
// says `policy.backoff.initial`, and a `pools[].policy` object where the
// document inlines the policy into the pool entry. Two public spellings of one
// concept, in a surface docs/reference/versioning.md declares stable, and
// nothing was gained by the second — the same object already spelled
// `interval`, `dryRun` and the whole `discovery` block exactly as the document
// does, because those parts embed the API type and the policy part did not.
//
// The API types rather than a view struct is what makes the names right by
// construction rather than by transcription. Durations render as strings
// through [v1alpha1.Duration]'s own marshaller, so time.Duration stays the
// Go-facing value.
type resolvedConfig struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Interval   *v1alpha1.Duration `json:"interval"`
	DryRun     bool               `json:"dryRun"`
	Discovery  v1alpha1.Discovery `json:"discovery"`
	// Policy is the resolved default: what every discovered pool gets unless
	// an entry below overrides it.
	Policy v1alpha1.Policy `json:"policy"`
	// Pools carries each override already resolved against that default, so a
	// reader need not apply the inheritance themselves. Inlined exactly as
	// v1alpha1.PoolOverride inlines it: the document has no `policy` key under
	// a pool entry.
	Pools []v1alpha1.PoolOverride `json:"pools,omitempty"`
}

// explicit renders a resolved policy as the document that would produce it,
// with nothing left to inherit.
//
// Every field is written, including the ones that happen to equal a default.
// The point of the report is to say what binpack will actually do, and a
// document that omits a value on the grounds that it is the current default
// stops being that the day the default moves.
func explicit(p v1alpha1.PoolPolicy) v1alpha1.Policy {
	// Never nil, and this is the one field where that matters. Every other
	// setting here is a scalar behind a pointer, where "set" and "unset" are
	// the pointer's own business; this one is a slice, and a nil one marshals
	// to JSON null, which reloads as an absent pointer and therefore as
	// "inherit". A pool that had cleared the global exclusions with
	// `namespaces: []` would come back excluding them again.
	//
	// Resolution cannot tell the two apart on binpack's behalf: SetDefaults
	// applies an override with `append([]string(nil), ...)`, and appending
	// nothing to nil is nil, so a cleared list and an unset one are the same
	// value by the time they reach here. What settles it is that this report
	// is the effective settings written out explicitly — "nothing is
	// excluded" is a fact about this binpack either way, and `[]` says it
	// while null asks the reader to guess.
	namespaces := p.ExcludedNamespaces
	if namespaces == nil {
		namespaces = []string{}
	}
	return v1alpha1.Policy{
		Enabled: &p.Enabled,
		Feasibility: v1alpha1.Feasibility{
			ExpendablePriorityCutoff: &p.ExpendablePriorityCutoff,
			ReserveForLargestPod:     &p.ReserveForLargestPod,
		},
		Autoscaler: v1alpha1.Autoscaler{
			SkipNodesWithLocalStorage:           &p.SkipNodesWithLocalStorage,
			SkipNodesWithSystemPods:             &p.SkipNodesWithSystemPods,
			BlockingSystemPodDistruptionTimeout: v1alpha1.NewDuration(p.BlockingSystemPodDistruptionTimeout),
		},
		Drain: v1alpha1.Drain{
			MaxPodsPerDrain: &p.MaxPodsPerDrain,
			StallTimeout:    v1alpha1.NewDuration(p.StallTimeout),
			RemovalTimeout:  v1alpha1.NewDuration(p.RemovalTimeout),
		},
		Backoff: v1alpha1.Backoff{
			Initial: v1alpha1.NewDuration(p.BackoffInitial),
			Max:     v1alpha1.NewDuration(p.BackoffMax),
		},
		Cooldown: v1alpha1.Cooldown{
			AfterScaleUp: v1alpha1.NewDuration(p.CooldownAfterScaleUp),
			AfterDrain:   v1alpha1.NewDuration(p.CooldownAfterDrain),
		},
		Exclusions: v1alpha1.Exclusions{Namespaces: &namespaces},
	}
}

func resolve(cfg *v1alpha1.Config) resolvedConfig {
	r := resolvedConfig{
		APIVersion: v1alpha1.GroupVersion,
		Kind:       v1alpha1.Kind,
		Interval:   cfg.Interval,
		DryRun:     *cfg.DryRun,
		Discovery:  cfg.Discovery,
		Policy:     explicit(cfg.PolicyFor()),
	}
	for _, pool := range cfg.Pools {
		r.Pools = append(r.Pools, v1alpha1.PoolOverride{
			Name:   pool.Name,
			Policy: explicit(cfg.PolicyFor(pool.Name)),
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
	for _, join := range cfg.Discovery.NodeGroups {
		p("  %s=%s is node group %s\n", cfg.Discovery.NodeGroupIDLabel,
			join.LabelValue, join.Group)
	}
	// Where binpack will look for the autoscaler, said before it has looked.
	// A wrong label key above fails preflight loudly; a wrong location here
	// produces a confident report that no cluster-autoscaler is running, and
	// nothing in that report is about configuration.
	//
	// Both halves are read from the resolved configuration, through the same
	// function the Get itself uses. Printing the constant name beside the
	// configured namespace was the original defect in miniature: a command
	// whose whole purpose is to show resolved settings, telling an operator
	// who renamed the object to go and look at one binpack will not read.
	p("autoscaler status: %s\n", statusRef(cfg))

	// The resolved default policy is what most pools will actually get, and
	// is far more useful to see than the sparse document that produced it.
	p("\ndefault policy for every discovered pool:\n")
	writePolicy(p, cfg.PolicyFor())

	for _, pool := range cfg.Pools {
		p("\noverride for pool %q:\n", pool.Name)
		writePolicy(p, cfg.PolicyFor(pool.Name))
	}
	// Under the overrides, because it is about them, and because the block
	// above is what makes the disclosure necessary: a misspelt name renders
	// exactly like a real one, fully resolved, down to the `enabled` an
	// operator switched off. The deployed binpack refuses to start on that
	// same document, so the command that exists to say what a document
	// resolves to must not be the one that reads as a clean bill of health.
	if len(cfg.Pools) > 0 {
		p("\n%s\n", poolNamesNotChecked)
	}
	// The sibling condition, separately. The joins are printed far above,
	// beside the label keys rather than down here with the overrides, and a
	// document can state one without the other — so a disclosure gated on the
	// overrides left the identical defect standing on the field next to it.
	if len(cfg.Discovery.NodeGroups) > 0 {
		if len(cfg.Pools) == 0 {
			p("\n")
		}
		p("%s\n", nodeGroupJoinsNotChecked)
	}

	return errors.Join(errs...)
}

func writePolicy(p func(string, ...any), policy v1alpha1.PoolPolicy) {
	p("  enabled:                 %t\n", policy.Enabled)
	p("  expendable below:        priority %d\n", policy.ExpendablePriorityCutoff)
	p("  reserve for largest pod: %t\n", policy.ReserveForLargestPod)
	// Rendered as what binpack believes about the autoscaler rather than as
	// three flag names, because that is the question an operator is asking
	// this command: not "what did I write" but "what will be assumed".
	p("  autoscaler skips nodes:  with local storage %t, with system pods %t\n",
		policy.SkipNodesWithLocalStorage, policy.SkipNodesWithSystemPods)
	if policy.BlockingSystemPodDistruptionTimeout == 0 {
		// Zero is a claim about the autoscaler, not an absent value — an
		// autoscaler older than 1.33 has no such grace — so it has to read as
		// a statement rather than as a blank.
		p("  system pods block for:   as long as they are there (no grace)\n")
	} else {
		p("  system pods block for:   %s after creation\n",
			policy.BlockingSystemPodDistruptionTimeout)
	}
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
