package auth

import "time"

// SelfTestOptions configures the scheduled credential self-test. Zero values are
// resolved to the package defaults so a caller that only sets Enabled still gets
// a workable loop.
type SelfTestOptions struct {
	// Enabled starts the schedule. The loop runs regardless so an operator can
	// trigger a run by hand while the schedule is off.
	Enabled bool
	// Interval is how often the schedule walks the credential pool.
	Interval time.Duration
	// PerCredentialPeriod is the minimum gap between two probes of one credential.
	PerCredentialPeriod time.Duration
	// Timeout bounds a single probe.
	Timeout time.Duration
	// Concurrency caps simultaneous probes.
	Concurrency int
	// QueryQuotaOnRateLimit asks the quota endpoint about a credential that
	// answered 429, so a spent window is told apart from upstream throttling.
	QueryQuotaOnRateLimit bool
	// QuotaConcurrency caps simultaneous quota lookups.
	QuotaConcurrency int
	// QuotaCacheTTL is how long a quota lookup result is reused.
	QuotaCacheTTL time.Duration
	// DeterministicFailureThreshold is how many consecutive failures that are the
	// credential's own fault escalate the cooldown. Transient failures, including
	// a probe that never reached the upstream, do not count toward it.
	DeterministicFailureThreshold int
	// MaxCooldown caps how far repeated failures push one credential's cooldown.
	MaxCooldown time.Duration
}

// DefaultSelfTestOptions mirrors the config package defaults. It is duplicated
// rather than imported because sdk/cliproxy/auth must not depend on
// internal/config: the SDK is embedded by external callers that never load a
// YAML file.
func DefaultSelfTestOptions() SelfTestOptions {
	return SelfTestOptions{
		Enabled:                       false,
		Interval:                      time.Minute,
		PerCredentialPeriod:           30 * time.Minute,
		Timeout:                       90 * time.Second,
		Concurrency:                   8,
		QueryQuotaOnRateLimit:         true,
		QuotaConcurrency:              4,
		QuotaCacheTTL:                 60 * time.Second,
		DeterministicFailureThreshold: 3,
		MaxCooldown:                   12 * time.Hour,
	}
}

// Normalize resolves unset options to their defaults so the loop never has to
// treat zero as a special case. A negative value is treated as unset rather than
// rejected: config validation is the place that reports bad input.
func (o SelfTestOptions) Normalize() SelfTestOptions {
	defaults := DefaultSelfTestOptions()
	if o.Interval <= 0 {
		o.Interval = defaults.Interval
	}
	if o.PerCredentialPeriod <= 0 {
		o.PerCredentialPeriod = defaults.PerCredentialPeriod
	}
	if o.Timeout <= 0 {
		o.Timeout = defaults.Timeout
	}
	if o.Concurrency <= 0 {
		o.Concurrency = defaults.Concurrency
	}
	if o.QuotaConcurrency <= 0 {
		o.QuotaConcurrency = defaults.QuotaConcurrency
	}
	if o.QuotaCacheTTL <= 0 {
		o.QuotaCacheTTL = defaults.QuotaCacheTTL
	}
	if o.DeterministicFailureThreshold <= 0 {
		o.DeterministicFailureThreshold = defaults.DeterministicFailureThreshold
	}
	if o.MaxCooldown <= 0 {
		o.MaxCooldown = defaults.MaxCooldown
	}
	return o
}
