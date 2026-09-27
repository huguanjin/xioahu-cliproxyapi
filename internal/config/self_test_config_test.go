package config

import "testing"

// The self-test options that default to on must not be turned off by a payload
// that simply does not mention them. This is the failure that matters: an
// operator who never writes the key would silently lose auto-disable, which is
// the only mechanism that takes a dead credential out of dispatch.
func TestSelfTestOptionalDefaultsSurviveAbsentKeys(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("port: 8317\n"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.SelfTest.AutoDisableOnThreshold == nil {
		t.Fatal("an absent auto-disable-on-threshold must keep a default, not read as nil")
	}
	if !*cfg.SelfTest.AutoDisableOnThreshold {
		t.Fatal("an absent auto-disable-on-threshold must default to true")
	}
	if cfg.SelfTest.DeepProbeEnabled == nil || !*cfg.SelfTest.DeepProbeEnabled {
		t.Fatal("an absent deep-probe-enabled must default to true")
	}
	if cfg.SelfTest.DeepProbeSamplePercent == nil || *cfg.SelfTest.DeepProbeSamplePercent != 2 {
		t.Fatal("an absent deep-probe-sample-percent must default to 2")
	}
	// This one is a plain bool and had the same bug on this path: absent must not
	// read as false.
	if !cfg.SelfTest.QueryQuotaOnRateLimit {
		t.Fatal("an absent query-quota-on-rate-limit must default to true")
	}
	if cfg.SelfTest.DeterministicFailureThreshold != 3 {
		t.Fatalf("an absent deterministic-failure-threshold must default to 3, got %d", cfg.SelfTest.DeterministicFailureThreshold)
	}
}

// An operator who writes false or 0 must get exactly that. The default must not
// override an explicit choice, and 0 for the sample must be expressible — as a
// plain int it was indistinguishable from an absent key.
func TestSelfTestOptionalExplicitValuesWin(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
self-test:
  auto-disable-on-threshold: false
  deep-probe-enabled: false
  deep-probe-sample-percent: 0
  query-quota-on-rate-limit: false
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if cfg.SelfTest.AutoDisableOnThreshold == nil {
		t.Fatal("an explicit false must survive as a non-nil pointer")
	}
	if *cfg.SelfTest.AutoDisableOnThreshold {
		t.Fatal("an explicit auto-disable-on-threshold: false must win over the default")
	}
	if cfg.SelfTest.DeepProbeEnabled == nil || *cfg.SelfTest.DeepProbeEnabled {
		t.Fatal("an explicit deep-probe-enabled: false must win over the default")
	}
	if cfg.SelfTest.DeepProbeSamplePercent == nil {
		t.Fatal("an explicit deep-probe-sample-percent: 0 must survive as a non-nil pointer")
	}
	if *cfg.SelfTest.DeepProbeSamplePercent != 0 {
		t.Fatalf("an explicit deep-probe-sample-percent: 0 must be honoured, got %d", *cfg.SelfTest.DeepProbeSamplePercent)
	}
	if cfg.SelfTest.QueryQuotaOnRateLimit {
		t.Fatal("an explicit query-quota-on-rate-limit: false must win over the default")
	}
}

// A partial block must not reset the fields it leaves out. This is the realistic
// shape of a hand-edited config: the operator sets one cadence and says nothing
// about the rest.
func TestSelfTestPartialBlockKeepsDefaults(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
self-test:
  enabled: true
  interval: 5m
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if !cfg.SelfTest.Enabled {
		t.Fatal("the explicit enabled: true must be honoured")
	}
	if cfg.SelfTest.Interval != "5m" {
		t.Fatalf("the explicit interval must be honoured, got %q", cfg.SelfTest.Interval)
	}
	if cfg.SelfTest.AutoDisableOnThreshold == nil || !*cfg.SelfTest.AutoDisableOnThreshold {
		t.Fatal("a partial self-test block must leave auto-disable on")
	}
	if cfg.SelfTest.MaxCooldown != "12h" {
		t.Fatalf("a partial block must keep the max-cooldown default, got %q", cfg.SelfTest.MaxCooldown)
	}
	if cfg.SelfTest.QuarantineProbePeriod != "8h" {
		t.Fatalf("a partial block must keep the quarantine default, got %q", cfg.SelfTest.QuarantineProbePeriod)
	}
}

// An out-of-range share is a config error, but an absent one is not: the two must
// not be conflated now that the field is a pointer.
func TestSelfTestSamplePercentValidation(t *testing.T) {
	if _, errParse := ParseConfigBytes([]byte("self-test:\n  deep-probe-sample-percent: 101\n")); errParse == nil {
		t.Fatal("a share above 100 must be rejected")
	}
	if _, errParse := ParseConfigBytes([]byte("self-test:\n  deep-probe-sample-percent: -1\n")); errParse == nil {
		t.Fatal("a negative share must be rejected")
	}
	if _, errParse := ParseConfigBytes([]byte("self-test:\n  deep-probe-sample-percent: 0\n")); errParse != nil {
		t.Fatalf("a share of 0 is valid and must be accepted, got %v", errParse)
	}
	if _, errParse := ParseConfigBytes([]byte("self-test:\n  enabled: true\n")); errParse != nil {
		t.Fatalf("an absent share is valid and must be accepted, got %v", errParse)
	}
}
