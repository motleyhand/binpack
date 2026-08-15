package v1alpha1

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// Load parses, defaults and validates a configuration document.
//
// Empty input is valid and yields the defaults, because pools and their
// bounds are discovered rather than declared: on a supported provider binpack
// needs no configuration at all to do something useful and safe.
func Load(data []byte) (*Config, error) {
	cfg := &Config{}

	// Strict: an unrecognised field is an error rather than a silent no-op.
	// A typo in configuration that governs node draining must not be
	// interpreted as "leave it at the default" — the operator believes they
	// have set something.
	if err := yaml.UnmarshalStrict(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing configuration: %w", err)
	}

	cfg.SetDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration:\n%w", err)
	}

	return cfg, nil
}

// Marshal renders a configuration document as YAML. Round-tripping through
// Load must be lossless, which the tests assert.
func Marshal(c *Config) ([]byte, error) {
	return yaml.Marshal(c)
}
