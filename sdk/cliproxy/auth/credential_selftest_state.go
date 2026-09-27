package auth

import (
	"strings"
	"time"
)

// SelfTestVerdict is the scheduling conclusion the self-test loop last reached
// for a credential. It is persisted so the cadence a credential earned survives
// a restart: without it a deploy would reset every backoff and re-probe the
// whole pool at once.
type SelfTestVerdict string

const (
	// SelfTestVerdictUnknown is the zero value: the credential has never been
	// probed, or its state predates this field.
	SelfTestVerdictUnknown SelfTestVerdict = ""
	// SelfTestVerdictHealthy means the most recent probe was accepted.
	SelfTestVerdictHealthy SelfTestVerdict = "healthy"
	// SelfTestVerdictCooling means the most recent probe failed deterministically
	// but below the auto-disable threshold.
	SelfTestVerdictCooling SelfTestVerdict = "cooling"
	// SelfTestVerdictQuarantine means the credential crossed the deterministic
	// failure threshold and was taken out of rotation. It keeps the quarantine
	// cadence so recovery is still detected.
	SelfTestVerdictQuarantine SelfTestVerdict = "quarantine"
	// SelfTestVerdictValidation means the account owner still has to complete
	// Google's verification step. It is not a failure and drives no strike, but it
	// is parked on a slower cadence because nothing changes until a human acts.
	SelfTestVerdictValidation SelfTestVerdict = "validation"
)

// SelfTestState is the persisted self-test scheduling state for one credential.
// It lives in Auth.Metadata under selfTestMetadataKey, which is what makes it
// survive a restart: the file store writes the whole Metadata map back to
// auths/*.json and reloads it verbatim.
//
// It is deliberately kept out of Auth's typed fields. Auth.Disabled is the only
// gate the dispatcher consults, so an auto-disabled credential has to set it to
// stop receiving traffic — but it must also stay probeable, which the dispatcher
// knows nothing about. AutoDisabled is that second dimension: the loop reads it
// to tell "an operator parked this" apart from "we parked this and can lift it
// ourselves".
type SelfTestState struct {
	// Strikes is the consecutive deterministic failure count. It is cleared by a
	// healthy probe, not by a cooldown expiring, so a credential that fails,
	// recovers, and fails again starts over rather than escalating forever.
	Strikes int `json:"strikes,omitempty"`
	// NextProbeAt is when this credential next enters the probe queue. Zero means
	// immediately due.
	NextProbeAt time.Time `json:"next_probe_at,omitempty"`
	// Verdict is the conclusion the last probe reached, which selects the cadence.
	Verdict SelfTestVerdict `json:"verdict,omitempty"`
	// AutoDisabled records that the self-test loop disabled this credential
	// rather than an operator. It is what keeps an auto-disabled credential
	// eligible for the low-frequency re-probe that can bring it back.
	AutoDisabled bool `json:"auto_disabled,omitempty"`
	// AutoDisableReason is the upstream message that triggered the auto-disable,
	// kept so an operator can tell why the pool dropped the credential.
	AutoDisableReason string `json:"auto_disable_reason,omitempty"`
}

// selfTestMetadataKey is the Auth.Metadata key holding the SelfTestState object.
const selfTestMetadataKey = "self_test"

// SelfTestState returns a copy of the credential's persisted self-test state.
// A missing or malformed object yields the zero value, which reads as "never
// probed, due now" — the safe direction, since it only costs one probe.
func (a *Auth) SelfTestState() SelfTestState {
	if a == nil || a.Metadata == nil {
		return SelfTestState{}
	}
	return decodeSelfTestState(a.Metadata[selfTestMetadataKey])
}

// SetSelfTestState writes the credential's self-test state back into its
// Metadata map, which is the structure the file store serialises. A zero state
// removes the key entirely rather than leaving an empty object behind: an
// untouched credential file should not grow a self_test block it never used.
func (a *Auth) SetSelfTestState(state SelfTestState) {
	if a == nil {
		return
	}
	if a.Metadata == nil {
		if state.IsZero() {
			return
		}
		a.Metadata = make(map[string]any)
	}
	if state.IsZero() {
		delete(a.Metadata, selfTestMetadataKey)
		return
	}
	a.Metadata[selfTestMetadataKey] = encodeSelfTestState(state)
}

// IsZero reports whether the state carries no information worth persisting.
func (s SelfTestState) IsZero() bool {
	return s.Strikes == 0 && s.NextProbeAt.IsZero() && s.Verdict == SelfTestVerdictUnknown && !s.AutoDisabled
}

// decodeSelfTestState reads a self_test object out of a Metadata value. It
// accepts the two shapes that can appear after a JSON round trip: the map form
// the file store produces on load, and the typed form a caller may have stored
// in-process. Anything else is reported as absent rather than guessed at, so a
// hand-edited credential file cannot park itself on a cadence.
func decodeSelfTestState(value any) SelfTestState {
	switch typed := value.(type) {
	case nil:
		return SelfTestState{}
	case SelfTestState:
		return typed
	case map[string]any:
		return decodeSelfTestStateMap(typed)
	default:
		return SelfTestState{}
	}
}

func decodeSelfTestStateMap(raw map[string]any) SelfTestState {
	state := SelfTestState{
		Strikes:           intFromMetadata(raw["strikes"]),
		NextProbeAt:       timeFromMetadata(raw["next_probe_at"]),
		Verdict:           SelfTestVerdict(stringFromMetadata(raw["verdict"])),
		AutoDisabled:      boolFromMetadata(raw["auto_disabled"]),
		AutoDisableReason: stringFromMetadata(raw["auto_disable_reason"]),
	}
	if state.Strikes < 0 {
		state.Strikes = 0
	}
	return state
}

// encodeSelfTestState renders a state as the plain value the JSON marshaler
// expects, so the on-disk shape is stable no matter which path wrote it.
func encodeSelfTestState(state SelfTestState) map[string]any {
	out := make(map[string]any, 5)
	if state.Strikes != 0 {
		out["strikes"] = state.Strikes
	}
	if !state.NextProbeAt.IsZero() {
		out["next_probe_at"] = state.NextProbeAt.UTC().Format(time.RFC3339)
	}
	if state.Verdict != SelfTestVerdictUnknown {
		out["verdict"] = string(state.Verdict)
	}
	if state.AutoDisabled {
		out["auto_disabled"] = true
	}
	if strings.TrimSpace(state.AutoDisableReason) != "" {
		out["auto_disable_reason"] = state.AutoDisableReason
	}
	return out
}

func stringFromMetadata(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func boolFromMetadata(value any) bool {
	flag, _ := value.(bool)
	return flag
}

// intFromMetadata accepts every numeric shape a JSON round trip can produce.
// Metadata is unmarshalled from JSON, where numbers arrive as float64, but a
// value set in-process may still be a Go int.
func intFromMetadata(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case float32:
		return int(typed)
	default:
		return 0
	}
}

// timeFromMetadata parses the RFC3339 string the encoder writes. An
// unparseable timestamp is treated as absent so a corrupt value cannot pin a
// credential to a future cadence forever.
func timeFromMetadata(value any) time.Time {
	text := stringFromMetadata(value)
	if text == "" {
		return time.Time{}
	}
	parsed, errParse := time.Parse(time.RFC3339, text)
	if errParse != nil {
		return time.Time{}
	}
	return parsed
}
