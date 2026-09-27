package auth

import (
	"strings"
	"time"
)

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
	// AutoDisableOnThreshold takes a credential out of rotation once it crosses
	// DeterministicFailureThreshold consecutive failures. It stays probeable on
	// the quarantine cadence, so a credential that recovers clears itself without
	// an operator.
	AutoDisableOnThreshold bool
	// QuarantineProbePeriod is the re-probe cadence for a credential that crossed
	// the threshold or was auto-disabled. It is the low-frequency end of the
	// ladder: slow enough not to waste probe budget on a dead account, frequent
	// enough to notice a recovery the same day.
	QuarantineProbePeriod time.Duration
	// ValidationProbePeriod is the re-probe cadence for a credential parked on a
	// validation 403. Only a human can clear it, so it is slower than the healthy
	// cadence but faster than quarantine, since an operator may act on the link.
	ValidationProbePeriod time.Duration
	// DeepProbeEnabled runs a second, generation-level probe on credentials that
	// passed the cheap check. Only a real generation call can reveal an exhausted
	// quota: the token and the authorization chain stay valid, so countTokens
	// keeps answering 2xx while inference is refused.
	DeepProbeEnabled bool
	// DeepProbeSamplePercent is the share of healthy credentials that get a deep
	// probe each sweep, so a credential whose quota ran out between recoveries is
	// still found. Credentials returning from cooldown or quarantine always get
	// one regardless of the sample.
	DeepProbeSamplePercent int
	// DeepProbeModel is the model the deep probe asks for. It must be a model the
	// provider serves cheaply; the probe only needs the upstream to admit or
	// refuse the call.
	DeepProbeModel string
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
		AutoDisableOnThreshold:        true,
		QuarantineProbePeriod:         8 * time.Hour,
		ValidationProbePeriod:         6 * time.Hour,
		DeepProbeEnabled:              true,
		DeepProbeSamplePercent:        2,
		DeepProbeModel:                "gemini-3.1-flash-lite",
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
	if o.QuarantineProbePeriod <= 0 {
		o.QuarantineProbePeriod = defaults.QuarantineProbePeriod
	}
	if o.ValidationProbePeriod <= 0 {
		o.ValidationProbePeriod = defaults.ValidationProbePeriod
	}
	if o.QuarantineProbePeriod > o.MaxCooldown && o.MaxCooldown > 0 {
		o.QuarantineProbePeriod = o.MaxCooldown
	}
	if o.ValidationProbePeriod > o.MaxCooldown && o.MaxCooldown > 0 {
		o.ValidationProbePeriod = o.MaxCooldown
	}
	if o.DeepProbeSamplePercent < 0 {
		o.DeepProbeSamplePercent = 0
	}
	if o.DeepProbeSamplePercent > 100 {
		o.DeepProbeSamplePercent = 100
	}
	if strings.TrimSpace(o.DeepProbeModel) == "" {
		o.DeepProbeModel = defaults.DeepProbeModel
	}
	return o
}

// BackoffForVerdict returns the gap until the next probe for a credential that
// last landed on the given verdict, given how many strikes it carries.
//
// The ladder is what keeps a large pool affordable: a healthy credential is
// re-checked every PerCredentialPeriod, and a failing one is checked
// progressively less often, so probe budget follows the credentials most likely
// to have changed rather than being spent evenly across the pool.
//
// Growth is exponential on the strike count and capped at MaxCooldown, so a
// credential that has been dead for a long time approaches the cap without ever
// being parked permanently. Quarantine and validation start at their own
// configured floors instead: both are states that take hours to change, so
// probing them on the healthy cadence would only burn requests.
func (o SelfTestOptions) BackoffForVerdict(verdict SelfTestVerdict, strikes int) time.Duration {
	o = o.Normalize()
	if strikes < 0 {
		strikes = 0
	}
	switch verdict {
	case SelfTestVerdictQuarantine:
		return clampDuration(shiftDuration(o.QuarantineProbePeriod, strikes), 0, o.MaxCooldown)
	case SelfTestVerdictValidation:
		return clampDuration(o.ValidationProbePeriod, 0, o.MaxCooldown)
	case SelfTestVerdictCooling:
		// One step past healthy for the first strike, then doubling.
		backoff := shiftDuration(o.PerCredentialPeriod, strikes+1)
		return clampDuration(backoff, 0, o.MaxCooldown)
	default:
		return clampDuration(o.PerCredentialPeriod, 0, o.MaxCooldown)
	}
}

// shiftDuration doubles base the given number of times, stopping at the point
// where another doubling would overflow or exceed any sane cooldown. The cap is
// applied by the caller; this only has to avoid wrapping into a negative
// duration, which would schedule a probe in the past and re-open the very loop
// the ladder exists to break.
func shiftDuration(base time.Duration, doublings int) time.Duration {
	if base <= 0 {
		return 0
	}
	for i := 0; i < doublings; i++ {
		const overflowGuard = time.Duration(1) << 62
		if base >= overflowGuard {
			return base
		}
		base *= 2
	}
	return base
}

func clampDuration(value, min, max time.Duration) time.Duration {
	if value < min {
		value = min
	}
	if max > 0 && value > max {
		value = max
	}
	return value
}
