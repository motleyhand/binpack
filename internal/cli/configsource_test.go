package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheConfigSourceIsAlwaysReported(t *testing.T) {
	// The failure this closes is a tool answering confidently about settings
	// it never read. Without the line, an explain run that silently fell back
	// to defaults is indistinguishable from one that read the deployed
	// configuration — and they answer different questions.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(
		"apiVersion: binpack.motleyhand.com/v1alpha1\nkind: BinpackConfig\ninterval: 7m0s\n"),
		0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path, want string
	}{
		{"a file given with -f", path, path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, source, err := loadConfigOrDefaults(tc.path, strings.NewReader(""))
			if err != nil {
				t.Fatalf("loadConfigOrDefaults: %v", err)
			}
			if cfg == nil {
				t.Fatal("no configuration returned")
			}
			if source != tc.want {
				t.Errorf("source: got %q, want %q", source, tc.want)
			}
		})
	}
}

func TestTheDeployedConfigIsUsedWithoutBeingAskedFor(t *testing.T) {
	// The whole point: `kubectl exec deploy/binpack -- binpack explain` must
	// answer about the binpack running beside it, not about one configured
	// with defaults. Those are different questions.
	dir := t.TempDir()
	mounted := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(mounted, []byte(
		"apiVersion: binpack.motleyhand.com/v1alpha1\nkind: BinpackConfig\ninterval: 7m0s\n"),
		0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func(orig string) func() {
		return func() { deployedConfig = orig }
	}(deployedConfig))
	deployedConfig = mounted

	cfg, source, err := loadConfigOrDefaults("", strings.NewReader(""))
	if err != nil {
		t.Fatalf("loadConfigOrDefaults: %v", err)
	}
	if source != mounted {
		t.Errorf("source: got %q, want the mounted file %q", source, mounted)
	}
	if got := cfg.Settings().Interval; got.String() != "7m0s" {
		t.Errorf("the mounted configuration was not applied: interval %s", got)
	}
}

func TestNothingGivenFallsBackAndSaysSo(t *testing.T) {
	t.Cleanup(func(orig string) func() {
		return func() { deployedConfig = orig }
	}(deployedConfig))
	deployedConfig = filepath.Join(t.TempDir(), "absent.yaml")

	cfg, source, err := loadConfigOrDefaults("", strings.NewReader(""))
	if err != nil {
		t.Fatalf("loadConfigOrDefaults: %v", err)
	}
	if cfg == nil {
		t.Fatal("no configuration returned")
	}
	if source != "built-in defaults" {
		t.Errorf("source: got %q, want the fallback named", source)
	}
}

func TestTheDefaultPathIsWhereTheChartMountsIt(t *testing.T) {
	// The default exists so `kubectl exec deploy/binpack -- binpack explain`
	// answers about the binpack running beside it. If the constant and the
	// chart ever disagree, it silently never applies in the one place it is
	// for, and explain goes back to answering about defaults.
	chart, err := os.ReadFile("../../charts/binpack/templates/deployment.yaml")
	if err != nil {
		t.Fatalf("reading the chart: %v", err)
	}
	if !strings.Contains(string(chart), "--file="+DeployedConfig) {
		t.Errorf("the chart does not pass --file=%s, so the constant and the "+
			"deployment can drift apart unnoticed", DeployedConfig)
	}
}

func TestAnUnreadableDeployedConfigIsNotSilentlyDefaulted(t *testing.T) {
	// Only "no such file" may fall back. A configuration that is present but
	// unreadable is a broken deployment, and answering with built-in defaults
	// would put the authority of the reported source behind settings nobody
	// chose — which is the exact failure the source line exists to prevent.
	//
	// A directory rather than a chmod, because a test running as root would
	// read a 0000 file quite happily and this must fail everywhere.
	t.Cleanup(func(orig string) func() {
		return func() { deployedConfig = orig }
	}(deployedConfig))
	deployedConfig = t.TempDir()

	cfg, source, err := loadConfigOrDefaults("", strings.NewReader(""))

	if err == nil {
		t.Fatalf("an unreadable configuration was accepted, reporting %q", source)
	}
	if cfg != nil {
		t.Error("a configuration was returned alongside the error")
	}
	if !strings.Contains(err.Error(), deployedConfig) {
		t.Errorf("the error does not name the file it could not read: %v", err)
	}
}
