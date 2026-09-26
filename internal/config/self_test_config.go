package config

import (
	"fmt"
	"time"
)

const (
	// DefaultSelfTestInterval is how often the schedule walks the credential pool.
	DefaultSelfTestInterval = time.Minute
	// DefaultSelfTestPerCredentialPeriod is the minimum gap between two probes of
	// the same credential.
	DefaultSelfTestPerCredentialPeriod = 30 * time.Minute
	// DefaultSelfTestTimeout bounds one probe. It is generous relative to a healthy
	// probe (milliseconds) because a large pool drains slowly, and cutting a slow
	// probe short would report a dead credential that is merely backed up.
	DefaultSelfTestTimeout = 90 * time.Second
	// DefaultSelfTestConcurrency caps simultaneous probes. The operator's 50 is
	// allowed but not defaulted: a pool that large multiplies both the burst and
	// the self-inflicted 429 rate.
	DefaultSelfTestConcurrency = 8
	// DefaultSelfTestQuotaConcurrency caps simultaneous quota lookups. Each lookup
	// forces an OAuth refresh, so it is far more expensive than a probe and must
	// stay well below it.
	DefaultSelfTestQuotaConcurrency = 4
	// DefaultSelfTestQuotaCacheTTL is how long a quota lookup result is reused.
	// It exists to collapse the burst of lookups that a pool-wide 429 produces.
	DefaultSelfTestQuotaCacheTTL = 60 * time.Second

	// SelfTestMaxConcurrency is the hard ceiling on probe concurrency.
	SelfTestMaxConcurrency = 50
	// SelfTestMaxTimeout is the hard ceiling on one probe's deadline.
	SelfTestMaxTimeout = 10 * time.Minute
)

// SelfTestConfig controls the scheduled credential self-test.
type SelfTestConfig struct {
	// Enabled starts the schedule at boot. It can also be toggled at runtime
	// through the management API, which is why the loop runs even when false.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Interval is how often the schedule walks the pool.
	Interval string `yaml:"interval" json:"interval"`
	// PerCredentialPeriod is the minimum gap between two probes of one credential.
	PerCredentialPeriod string `yaml:"per-credential-period" json:"per-credential-period"`
	// Timeout bounds a single probe.
	Timeout string `yaml:"timeout" json:"timeout"`
	// Concurrency caps simultaneous probes.
	Concurrency int `yaml:"concurrency" json:"concurrency"`
	// QueryQuotaOnRateLimit asks the quota endpoint about a credential that
	// answered 429, so a spent window is told apart from upstream throttling.
	QueryQuotaOnRateLimit bool `yaml:"query-quota-on-rate-limit" json:"query-quota-on-rate-limit"`
	// QuotaConcurrency caps simultaneous quota lookups.
	QuotaConcurrency int `yaml:"quota-concurrency" json:"quota-concurrency"`
	// QuotaCacheTTL is how long a quota lookup result is reused.
	QuotaCacheTTL string `yaml:"quota-cache-ttl" json:"quota-cache-ttl"`
	// DeterministicFailureThreshold is how many consecutive failures that are the
	// credential's own fault (a missing or rejected refresh token, or a spent
	// quota window) escalate the cooldown. Transient failures do not count.
	DeterministicFailureThreshold int `yaml:"deterministic-failure-threshold" json:"deterministic-failure-threshold"`
	// MaxCooldown caps how far repeated failures push one credential's cooldown.
	MaxCooldown string `yaml:"max-cooldown" json:"max-cooldown"`
}

// DefaultSelfTestConfig returns the self-test defaults. The feature ships
// disabled: it sends real upstream requests on a timer, and an operator should
// opt in after deciding the pool can absorb them.
func DefaultSelfTestConfig() SelfTestConfig {
	return SelfTestConfig{
		Enabled:                       false,
		Interval:                      DefaultSelfTestInterval.String(),
		PerCredentialPeriod:           DefaultSelfTestPerCredentialPeriod.String(),
		Timeout:                       DefaultSelfTestTimeout.String(),
		Concurrency:                   DefaultSelfTestConcurrency,
		QueryQuotaOnRateLimit:         true,
		QuotaConcurrency:              DefaultSelfTestQuotaConcurrency,
		QuotaCacheTTL:                 DefaultSelfTestQuotaCacheTTL.String(),
		DeterministicFailureThreshold: 3,
		MaxCooldown:                   "12h",
	}
}

// Durations parses and validates the self-test durations.
func (c SelfTestConfig) Durations() (time.Duration, time.Duration, time.Duration, time.Duration, time.Duration, error) {
	interval, errInterval := parsePositiveDuration(c.Interval, "self-test.interval")
	if errInterval != nil {
		return 0, 0, 0, 0, 0, errInterval
	}
	perCredential, errPeriod := parsePositiveDuration(c.PerCredentialPeriod, "self-test.per-credential-period")
	if errPeriod != nil {
		return 0, 0, 0, 0, 0, errPeriod
	}
	timeout, errTimeout := parsePositiveDuration(c.Timeout, "self-test.timeout")
	if errTimeout != nil {
		return 0, 0, 0, 0, 0, errTimeout
	}
	if timeout > SelfTestMaxTimeout {
		return 0, 0, 0, 0, 0, fmt.Errorf("self-test.timeout must not exceed %s", SelfTestMaxTimeout)
	}
	cacheTTL, errCache := parsePositiveDuration(c.QuotaCacheTTL, "self-test.quota-cache-ttl")
	if errCache != nil {
		return 0, 0, 0, 0, 0, errCache
	}
	maxCooldown, errCooldown := parsePositiveDuration(c.MaxCooldown, "self-test.max-cooldown")
	if errCooldown != nil {
		return 0, 0, 0, 0, 0, errCooldown
	}
	return interval, perCredential, timeout, cacheTTL, maxCooldown, nil
}

// Validate verifies the self-test bounds.
func (c SelfTestConfig) Validate() error {
	if _, _, _, _, _, errDurations := c.Durations(); errDurations != nil {
		return errDurations
	}
	// A non-positive concurrency is treated as unset and resolved to the default,
	// so only an explicit value above the ceiling is rejected.
	if c.Concurrency > SelfTestMaxConcurrency {
		return fmt.Errorf("self-test.concurrency must not exceed %d", SelfTestMaxConcurrency)
	}
	if c.QuotaConcurrency > SelfTestMaxConcurrency {
		return fmt.Errorf("self-test.quota-concurrency must not exceed %d", SelfTestMaxConcurrency)
	}
	if c.DeterministicFailureThreshold < 0 {
		return fmt.Errorf("self-test.deterministic-failure-threshold must not be negative")
	}
	return nil
}

// Effective returns the config with unset scalars resolved to their defaults, so
// callers never have to treat zero as a separate case.
func (c SelfTestConfig) Effective() SelfTestConfig {
	defaults := DefaultSelfTestConfig()
	if c.Concurrency <= 0 {
		c.Concurrency = defaults.Concurrency
	}
	if c.QuotaConcurrency <= 0 {
		c.QuotaConcurrency = defaults.QuotaConcurrency
	}
	if c.DeterministicFailureThreshold <= 0 {
		c.DeterministicFailureThreshold = defaults.DeterministicFailureThreshold
	}
	if c.Interval == "" {
		c.Interval = defaults.Interval
	}
	if c.PerCredentialPeriod == "" {
		c.PerCredentialPeriod = defaults.PerCredentialPeriod
	}
	if c.Timeout == "" {
		c.Timeout = defaults.Timeout
	}
	if c.QuotaCacheTTL == "" {
		c.QuotaCacheTTL = defaults.QuotaCacheTTL
	}
	if c.MaxCooldown == "" {
		c.MaxCooldown = defaults.MaxCooldown
	}
	return c
}

func parsePositiveDuration(value string, field string) (time.Duration, error) {
	parsed, errParse := time.ParseDuration(value)
	if errParse != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", field)
	}
	return parsed, nil
}
