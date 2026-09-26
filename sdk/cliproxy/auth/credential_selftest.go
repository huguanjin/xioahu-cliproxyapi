package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// CredentialSelfTestResult carries the outcome of one credential probe. It is
// deliberately narrower than Result: a probe that succeeds must not clear
// cooldown state, because a healthy answer to a cheap endpoint says nothing
// about whether the credential was actually rejected upstream.
type CredentialSelfTestResult struct {
	// AuthID identifies the probed credential.
	AuthID string
	// Provider is copied for logging and hook emission.
	Provider string
	// StatusCode is the upstream HTTP status; 0 means the probe never reached
	// the upstream (dial error, timeout) and carries no verdict.
	StatusCode int
	// Message is the upstream body or transport error, trimmed for logging.
	Message string
	// RetryAfter carries a provider supplied retry hint.
	RetryAfter *time.Duration
	// ProbeError marks a rejection the executor reported as an error while
	// acquiring a token rather than as an upstream response body. The status is
	// still a real verdict from the upstream, so it counts toward the
	// deterministic strikes, but the request never reached the inference path
	// and must not advance the payment ladder.
	ProbeError bool
}

// selfTestStrikes counts consecutive deterministic failures per auth. Unlike the
// credential_invalid counter, an entry here only ever drives a longer cooldown,
// never a disable, so it is safe to keep across a temporary recovery: it is
// cleared by a successful probe, not by the window expiring.
var selfTestStrikes sync.Map

// SelfTestStrikeCount reports the current consecutive deterministic failure
// count for an auth, for reporting and tests.
func SelfTestStrikeCount(authID string) int {
	value, ok := selfTestStrikes.Load(strings.TrimSpace(authID))
	if !ok {
		return 0
	}
	counter, ok := value.(*atomic.Int64)
	if !ok || counter == nil {
		return 0
	}
	count := counter.Load()
	if count < 0 {
		return 0
	}
	return int(count)
}

// resetSelfTestStrikes clears the counter for an auth, used when it answers
// healthily again.
func resetSelfTestStrikes(authID string) {
	selfTestStrikes.Delete(strings.TrimSpace(authID))
}

// bumpSelfTestStrikes increments and returns the counter for an auth.
func bumpSelfTestStrikes(authID string) int {
	value, _ := selfTestStrikes.LoadOrStore(strings.TrimSpace(authID), new(atomic.Int64))
	counter, ok := value.(*atomic.Int64)
	if !ok || counter == nil {
		return 0
	}
	count := counter.Add(1)
	if count < 0 {
		return 0
	}
	return int(count)
}

// ApplyCredentialSelfTest folds one probe outcome into an auth's cooldown state
// and returns the failure it recorded, or a zero value when the probe carried no
// verdict.
//
// Only rejections that a real request would also treat as credential failures are
// recorded. The intent is to drain a dead credential out of rotation before a
// user request discovers it, so the probe drives the same payment ladder a live
// 403 drives — it does not drive the credential_invalid disable path, which stays
// reserved for failures observed while serving real traffic.
//
// A successful probe is intentionally not treated as a success for scheduling
// purposes: clearing the ladders on a cheap endpoint's answer would let one flaky
// probe reset a credential that live traffic is still failing. It does clear the
// deterministic strike counter, because a credential that answers is not dead.
func (m *Manager) ApplyCredentialSelfTest(ctx context.Context, result CredentialSelfTestResult) SelfTestFailure {
	if m == nil {
		return SelfTestFailure{}
	}
	authID := strings.TrimSpace(result.AuthID)
	if authID == "" {
		return SelfTestFailure{}
	}
	// A 2xx answer clears the strikes: the credential reached the upstream and
	// was accepted.
	if result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices {
		resetSelfTestStrikes(authID)
		return SelfTestFailure{}
	}
	// StatusCode 0 means the probe never got an answer, which is not a verdict on
	// the credential at all.
	if result.StatusCode == 0 {
		return SelfTestFailure{}
	}
	kind, ok := classifySelfTestStatus(result.StatusCode)
	if !ok {
		// Transient upstream trouble (5xx, 408, connection resets) is not a
		// verdict on the credential, so the probe stays silent rather than
		// cooling a healthy credential down.
		return SelfTestFailure{
			AuthID:     authID,
			Provider:   result.Provider,
			StatusCode: result.StatusCode,
			Message:    credentialSelfTestMessage(result),
			Kind:       SelfTestFailureTransient,
		}
	}

	strikes := bumpSelfTestStrikes(authID)
	now := time.Now()
	var failure SelfTestFailure
	cooldownStateChanged := false
	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		// The credential went away mid-probe; drop the strike we just counted so
		// a re-registered file does not inherit it.
		selfTestStrikes.Delete(authID)
		return SelfTestFailure{}
	}
	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}
	probeErr := &Error{
		Message:    credentialSelfTestMessage(result),
		HTTPStatus: result.StatusCode,
		Retryable:  false,
	}
	// The probe reports at the credential level, so no per-model state exists to
	// aggregate. Calling updateAggregatedAvailability here would take its
	// no-model-states path, which clears the very cooldown just written.
	//
	// A token-acquisition rejection stops short of the payment ladder: the
	// credential was refused before any inference call, so there is no window to
	// cool down — the fix is a new token, not waiting. The strikes below still
	// escalate, which is what actually parks a credential whose refresh token is
	// gone.
	if !result.ProbeError {
		applyAuthFailureState(auth, probeErr, result.RetryAfter, now, m.cooldownDisabledForAuth(auth))
	}
	options := m.selfTestOptions()
	escalated := false
	if strikes >= options.DeterministicFailureThreshold {
		escalated = extendSelfTestCooldown(auth, strikes, options.MaxCooldown, now)
	}
	_ = m.persist(ctx, auth)
	authSnapshot := auth.Clone()
	if trackCooldownState {
		cooldownRecordsAfter := m.cooldownStateRecordsForAuthLocked(auth, now)
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, cooldownRecordsAfter)
	}
	failure = SelfTestFailure{
		AuthID:        authID,
		Provider:      authSnapshot.Provider,
		Label:         authSnapshot.Label,
		StatusCode:    result.StatusCode,
		Message:       credentialSelfTestMessage(result),
		Kind:          kind,
		Strikes:       strikes,
		CooldownUntil: authSnapshot.NextRetryAfter,
	}
	m.mu.Unlock()

	if m.scheduler != nil {
		m.scheduler.upsertAuth(authSnapshot)
	}
	if cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}

	if escalated {
		log.Warnf("credential self-test: %s %s failed %d times deterministically, cooldown extended to %v (%s)",
			authSnapshot.Provider, authID, strikes, authSnapshot.NextRetryAfter, authSnapshot.StatusMessage)
	} else {
		log.Infof("credential self-test: %s %s returned %d, cooldown reason=%q until %v",
			authSnapshot.Provider, authID, result.StatusCode, authSnapshot.Quota.Reason, authSnapshot.NextRetryAfter)
	}
	// No model is attached: the probe reports on the credential, not on any one
	// model, so the event carries only the auth-level cooldown.
	probeResult := Result{
		AuthID:   authID,
		Provider: authSnapshot.Provider,
		Success:  false,
		Error:    probeErr,
	}
	m.publishErrorEvent(probeResult, authSnapshot)
	m.hook.OnResult(ctx, probeResult)
	return failure
}

// classifySelfTestStatus maps an upstream rejection to a failure kind. The bool
// is false when the status says nothing about the credential's validity, in
// which case the probe must leave cooldown state alone.
//
// 404 is treated as transient on purpose: for these endpoints it usually means we
// asked the wrong regional base or the upstream moved the path, neither of which
// is the credential's fault.
func classifySelfTestStatus(status int) (SelfTestFailureKind, bool) {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return SelfTestFailureDeterministic, true
	default:
		return SelfTestFailureTransient, false
	}
}

// extendSelfTestCooldown pushes a repeatedly failing credential's cooldown out
// toward maxCooldown without touching its backoff levels.
//
// The payment ladder already advances once per window on its own; this only
// overrides the single deadline the scheduler reads, so a credential that has
// failed the threshold number of times in a row is parked for a long, visible
// interval instead of being retried on the ladder's short early steps. It never
// disables: an operator decides that, and a credential that is merely exhausted
// looks identical to a dead one from here.
//
// The extension grows monotonically with the strike count and approaches
// maxCooldown asymptotically, so a credential that has been dead for a long time
// stops consuming probe budget without ever being parked permanently.
func extendSelfTestCooldown(auth *Auth, strikes int, maxCooldown time.Duration, now time.Time) bool {
	if auth == nil || maxCooldown <= 0 || strikes <= 0 {
		return false
	}
	extended := maxCooldown * time.Duration(strikes) / time.Duration(strikes+1)
	if extended < time.Minute {
		extended = time.Minute
	}
	if extended > maxCooldown {
		extended = maxCooldown
	}
	until := now.Add(extended)
	if !auth.NextRetryAfter.Before(until) {
		// Already parked at least this far out; do not shorten it.
		return false
	}
	auth.NextRetryAfter = until
	auth.Unavailable = true
	auth.Quota.Exceeded = true
	if auth.Quota.NextRecoverAt.Before(until) {
		auth.Quota.NextRecoverAt = until
	}
	auth.UpdatedAt = now
	return true
}

func credentialSelfTestMessage(result CredentialSelfTestResult) string {
	if message := strings.TrimSpace(result.Message); message != "" {
		const maxMessageBytes = 512
		if len(message) > maxMessageBytes {
			return message[:maxMessageBytes]
		}
		return message
	}
	return "scheduled credential self-test failed"
}
