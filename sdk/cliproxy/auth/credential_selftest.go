package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// CredentialSelfTestTier identifies which probe produced a result. The tier
// decides how much scheduling authority the answer carries: the cheap check
// speaks for the credential's authorization, while only a real generation call
// can speak for its quota.
type CredentialSelfTestTier int

const (
	// SelfTestTierAuthorization is the cheap check: it asks the upstream whether
	// this credential may call at all. A healthy answer here does not prove the
	// credential can serve traffic, because an exhausted quota rejects generation
	// while leaving authorization intact.
	SelfTestTierAuthorization CredentialSelfTestTier = 1
	// SelfTestTierGeneration is the deep check: a minimal real generation call,
	// which is the only probe an exhausted quota can fail.
	SelfTestTierGeneration CredentialSelfTestTier = 2
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
	// ForbiddenType carries the subtype an executor read out of a 403 body, when
	// it can tell them apart. "validation" means the account owner can still fix
	// it by completing Google's verification; "violation" means a terms-of-service
	// ban. Empty means the executor made no distinction.
	ForbiddenType string
	// ValidationURL is the verification or appeal link the upstream sent with a
	// validation 403, so the operator can act on it without reading the raw body.
	ValidationURL string
	// Tier is which probe produced this result. It defaults to
	// SelfTestTierAuthorization so a caller that does not distinguish tiers keeps
	// the cheaper probe's meaning.
	Tier CredentialSelfTestTier
}

// ApplyCredentialSelfTest folds one probe outcome into a credential's persisted
// scheduling state and returns the failure it recorded, or a zero value when the
// probe carried no verdict.
//
// It is the only place self-test state changes, so every scheduling decision —
// the strike count, the verdict, the next probe time, and whether the credential
// is auto-disabled — is made here and nowhere else. That keeps the ladder
// consistent no matter which probe produced the answer.
func (m *Manager) ApplyCredentialSelfTest(ctx context.Context, result CredentialSelfTestResult) SelfTestFailure {
	if m == nil {
		return SelfTestFailure{}
	}
	authID := strings.TrimSpace(result.AuthID)
	if authID == "" {
		return SelfTestFailure{}
	}
	tier := result.Tier
	if tier == 0 {
		tier = SelfTestTierAuthorization
	}

	if result.StatusCode >= http.StatusOK && result.StatusCode < http.StatusMultipleChoices {
		return m.applySelfTestSuccess(ctx, authID, tier)
	}
	// StatusCode 0 means the probe never got an answer, which is not a verdict on
	// the credential at all.
	if result.StatusCode == 0 {
		return SelfTestFailure{}
	}
	kind, ok := classifySelfTestStatus(result.StatusCode, result.ForbiddenType)
	if !ok {
		// Transient upstream trouble (5xx, 408, connection resets) is not a verdict
		// on the credential. The probe is still reported so an operator can see the
		// upstream is unwell, but no scheduling state moves for it — and crucially
		// the next probe time is left alone, so one bad sweep cannot drag the whole
		// pool onto a failing credential's cadence.
		return SelfTestFailure{
			AuthID:        authID,
			Provider:      result.Provider,
			StatusCode:    result.StatusCode,
			Message:       credentialSelfTestMessage(result),
			Kind:          SelfTestFailureTransient,
			ForbiddenType: result.ForbiddenType,
			ValidationURL: result.ValidationURL,
		}
	}

	// A validation 403 is not a dead credential: the account owner can complete
	// the verification step and the credential comes back, and its quota endpoint
	// often keeps answering in the meantime. It is parked on its own slow cadence
	// and carries no strike, because only a human can clear it.
	if kind == SelfTestFailureValidation {
		return m.applySelfTestValidation(ctx, authID, result)
	}

	failure := m.applySelfTestFailure(ctx, authID, result, kind)
	if failure.AuthID != "" && failure.ForbiddenType == "" {
		failure.ForbiddenType = result.ForbiddenType
	}
	return failure
}

// applySelfTestSuccess records a healthy probe: the strikes are cleared, the
// cadence returns to the healthy step, and a credential the loop had disabled
// itself is put back into rotation.
//
// Only a deep probe clears the auth-level cooldown. A credential that answers
// the cheap authorization check may still be serving a cooldown the dispatcher
// put there on behalf of live traffic, and a probe is not evidence about that
// traffic; a credential that completed a real generation call is.
func (m *Manager) applySelfTestSuccess(ctx context.Context, authID string, tier CredentialSelfTestTier) SelfTestFailure {
	options := m.selfTestOptions()
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return SelfTestFailure{}
	}
	state := auth.SelfTestState()
	state.Strikes = 0
	state.Verdict = SelfTestVerdictHealthy
	state.NextProbeAt = now.Add(options.BackoffForVerdict(SelfTestVerdictHealthy, 0))

	recovered := false
	if state.AutoDisabled {
		// The loop parked this credential and the loop is the only thing that may
		// lift it. An operator-disabled credential never reaches here, because its
		// AutoDisabled flag is false and the loop does not probe it.
		state.AutoDisabled = false
		state.AutoDisableReason = ""
		auth.Disabled = false
		auth.Status = StatusActive
		auth.StatusMessage = ""
		recovered = true
	}
	auth.SetSelfTestState(state)
	if tier == SelfTestTierGeneration {
		clearAuthStateOnSuccess(auth, now)
	}
	_ = m.persist(ctx, auth)
	snapshot := auth.Clone()
	m.mu.Unlock()

	m.afterSelfTestStateChange(snapshot)
	if recovered {
		log.Warnf("credential self-test: %s %s answered again and was returned to rotation", snapshot.Provider, authID)
	}
	return SelfTestFailure{}
}

// applySelfTestValidation parks a credential whose account still needs Google
// verification. It records the verdict and the slow cadence but touches neither
// the strikes nor the cooldown: cooling a recoverable account down is exactly the
// false positive the probe exists to avoid.
func (m *Manager) applySelfTestValidation(ctx context.Context, authID string, result CredentialSelfTestResult) SelfTestFailure {
	options := m.selfTestOptions()
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return SelfTestFailure{}
	}
	state := auth.SelfTestState()
	state.Verdict = SelfTestVerdictValidation
	state.NextProbeAt = now.Add(options.BackoffForVerdict(SelfTestVerdictValidation, state.Strikes))
	auth.SetSelfTestState(state)
	_ = m.persist(ctx, auth)
	snapshot := auth.Clone()
	m.mu.Unlock()

	m.afterSelfTestStateChange(snapshot)
	return SelfTestFailure{
		AuthID:        authID,
		Provider:      snapshot.Provider,
		Label:         snapshot.Label,
		StatusCode:    result.StatusCode,
		Message:       credentialSelfTestMessage(result),
		Kind:          SelfTestFailureValidation,
		Strikes:       snapshot.SelfTestState().Strikes,
		ForbiddenType: result.ForbiddenType,
		ValidationURL: result.ValidationURL,
	}
}

// applySelfTestFailure records a deterministic rejection: it advances the strike
// count, extends the cooldown, and takes the credential out of rotation once the
// threshold is crossed.
func (m *Manager) applySelfTestFailure(ctx context.Context, authID string, result CredentialSelfTestResult, kind SelfTestFailureKind) SelfTestFailure {
	options := m.selfTestOptions()
	now := time.Now()

	m.mu.Lock()
	auth, ok := m.auths[authID]
	if !ok || auth == nil {
		m.mu.Unlock()
		return SelfTestFailure{}
	}
	var cooldownRecordsBefore []CooldownStateRecord
	trackCooldownState := m.cooldownStore != nil
	if trackCooldownState {
		cooldownRecordsBefore = m.cooldownStateRecordsForAuthLocked(auth, now)
	}
	cooldownStateChanged := false

	state := auth.SelfTestState()
	state.Strikes++
	strikes := state.Strikes
	verdict := SelfTestVerdictCooling
	autoDisabled := false
	if strikes >= options.DeterministicFailureThreshold {
		verdict = SelfTestVerdictQuarantine
		autoDisabled = options.AutoDisableOnThreshold
	}
	state.Verdict = verdict
	state.NextProbeAt = now.Add(options.BackoffForVerdict(verdict, strikes))
	if autoDisabled {
		state.AutoDisabled = true
		state.AutoDisableReason = credentialSelfTestMessage(result)
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
	// cool down — the fix is a new token, not waiting.
	if !result.ProbeError {
		applyAuthFailureState(auth, probeErr, result.RetryAfter, now, m.cooldownDisabledForAuth(auth))
	}
	extendSelfTestCooldown(auth, strikes, options.MaxCooldown, now)

	if autoDisabled {
		// Setting Disabled is what actually stops the dispatcher handing this
		// credential live traffic; AutoDisabled only records that we were the ones
		// who did it, so the re-probe below can lift it again.
		auth.Disabled = true
		auth.Status = StatusDisabled
		auth.StatusMessage = state.AutoDisableReason
		auth.Unavailable = false
	}

	auth.SetSelfTestState(state)
	_ = m.persist(ctx, auth)
	snapshot := auth.Clone()
	if trackCooldownState {
		cooldownStateChanged = !cooldownStateRecordsEqual(cooldownRecordsBefore, m.cooldownStateRecordsForAuthLocked(auth, now))
	}
	m.mu.Unlock()

	failure := SelfTestFailure{
		AuthID:        authID,
		Provider:      snapshot.Provider,
		Label:         snapshot.Label,
		StatusCode:    result.StatusCode,
		Message:       credentialSelfTestMessage(result),
		Kind:          kind,
		Strikes:       strikes,
		CooldownUntil: snapshot.NextRetryAfter,
		ForbiddenType: result.ForbiddenType,
		ValidationURL: result.ValidationURL,
	}
	m.afterSelfTestStateChange(snapshot)
	if trackCooldownState && cooldownStateChanged {
		m.persistCooldownStates(context.Background())
	}

	if autoDisabled {
		log.Warnf("credential self-test: %s %s failed %d times deterministically and was disabled automatically; re-probing every %v",
			snapshot.Provider, authID, strikes, options.QuarantineProbePeriod)
	} else if strikes >= options.DeterministicFailureThreshold {
		log.Warnf("credential self-test: %s %s failed %d times deterministically, cooldown extended to %v (%s)",
			snapshot.Provider, authID, strikes, snapshot.NextRetryAfter, snapshot.StatusMessage)
	} else {
		log.Infof("credential self-test: %s %s returned %d, cooldown reason=%q until %v",
			snapshot.Provider, authID, result.StatusCode, snapshot.Quota.Reason, snapshot.NextRetryAfter)
	}
	// No model is attached: the probe reports on the credential, not on any one
	// model, so the event carries only the auth-level cooldown.
	probeResult := Result{
		AuthID:   authID,
		Provider: snapshot.Provider,
		Success:  false,
		Error:    probeErr,
	}
	m.publishErrorEvent(probeResult, snapshot)
	m.hook.OnResult(ctx, probeResult)
	return failure
}

// afterSelfTestStateChange republishes a credential whose scheduling state the
// probe just moved, so the scheduler stops selecting a credential that was taken
// out of rotation without waiting for the next rebuild.
func (m *Manager) afterSelfTestStateChange(snapshot *Auth) {
	if m == nil || snapshot == nil {
		return
	}
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
}

// classifySelfTestStatus maps an upstream rejection to a failure kind. The bool
// is false when the status says nothing about the credential's validity, in
// which case the probe must leave cooldown state alone.
//
// 404 is treated as transient on purpose: for these endpoints it usually means we
// asked the wrong regional base or the upstream moved the path, neither of which
// is the credential's fault.
//
// A 403 is split by the subtype the executor read out of the body. Only the
// unambiguous "validation" case is separated out; anything else, including a
// 403 whose body named no subtype, stays deterministic. Erring toward
// deterministic keeps an unclassifiable rejection from being silently excused,
// which is the safer direction for a credential that may genuinely be dead.
func classifySelfTestStatus(status int, forbiddenType string) (SelfTestFailureKind, bool) {
	switch status {
	case http.StatusForbidden:
		if forbiddenType == string(SelfTestFailureValidation) {
			return SelfTestFailureValidation, true
		}
		return SelfTestFailureDeterministic, true
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusTooManyRequests:
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
// interval instead of being retried on the ladder's short early steps.
//
// The extension grows monotonically with the strike count and approaches
// maxCooldown asymptotically, so a credential that has been dead for a long time
// stops consuming probe budget without ever being parked permanently.
//
// It no longer drives the probe cadence. Unavailable and Quota.Exceeded are what
// the dispatcher skips a credential for; treating them as the probe's own skip
// condition is what previously made a failing credential progressively less
// likely to ever be examined again.
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
