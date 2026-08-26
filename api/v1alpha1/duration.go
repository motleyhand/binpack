package v1alpha1

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration that marshals as a Go duration string ("60s",
// "10m") rather than an integer count of nanoseconds.
//
// It exists for the wire representation, not for the dependency. A duration
// written as "10m" is what an operator types and what survives promotion to a
// CustomResourceDefinition unchanged — metav1.Duration marshals identically,
// so swapping to it then would be invisible to anyone who has written a
// config file, and swapping to a nanosecond count would not.
//
// The reason this comment used to give was that the type keeps
// k8s.io/apimachinery out of the configuration API, and that is not true:
// validation.go imports k8s.io/apimachinery/pkg/util/validation directly, for
// the label-key and namespace rules it argues for on their own merits, and
// k8s.io/klog/v2 is in the closure of that import. Naming a dependency the
// package already has is not a reason to keep a bespoke type — it is a reason
// somebody proposing metav1.Duration gets refused by an argument that has
// stopped holding.
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
