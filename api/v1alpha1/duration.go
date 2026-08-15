package v1alpha1

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that marshals as a Go duration string ("60s",
// "10m") rather than an integer count of nanoseconds.
//
// This exists rather than metav1.Duration so the configuration API does not
// drag in k8s.io/apimachinery. The wire representation is identical, so
// swapping to metav1.Duration when this becomes a CustomResourceDefinition
// would be invisible to anyone who has written a config file.
type Duration struct {
	time.Duration
}

// NewDuration is a convenience for tests and defaults.
func NewDuration(d time.Duration) *Duration { return &Duration{Duration: d} }

// UnmarshalJSON accepts a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"60s\" or \"10m\": %w", err)
	}

	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}

	d.Duration = parsed
	return nil
}

// MarshalJSON emits a duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}
