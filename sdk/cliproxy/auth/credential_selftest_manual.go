package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Errors a manual credential test can report. They are separate from a probe
// failure: each one means the test never reached the question it was asked.
var (
	ErrCredentialTestUnavailable = errors.New("credential test is unavailable")
	ErrCredentialTestNotFound    = errors.New("credential not found")
)

// CredentialTestResult is the outcome of an on-demand connectivity test on one
// credential, started by an operator rather than the schedule.
type CredentialTestResult struct {
	AuthID   string
	Provider string
	Label    string
	// Probed is false when the provider has no credential-check endpoint, or when
	// the probe could not be run at all. It is what separates "we asked and the
	// answer was good" from "we never got to ask".
	Probed bool
	// Healthy is only meaningful when Probed is true.
	Healthy bool
	// StatusCode is the upstream status; 0 means the probe never reached it.
	StatusCode int
	// Message is the upstream body or transport error, already trimmed.
	Message string
	// Tier is which probe produced this answer: 1 is the cheap authorization
	// check, 2 a real generation call.
	Tier CredentialSelfTestTier
	// ForbiddenType and ValidationURL carry what the executor read out of a 403.
	ForbiddenType string
	ValidationURL string
	// Duration is how long the probe took, so an operator can tell a slow
	// credential from a dead one.
	Duration time.Duration
	// Err carries the reason a probe could not run. It is separate from a
	// rejection: an error here means no verdict was reached.
	Err error
	// Verdict and Strikes are the scheduling state as it stood when the test ran.
	// They are reported, not changed: the scheduled loop owns that state. They let
	// the UI show "alive, but the loop has it quarantined" without this call
	// altering either.
	Verdict string
	Strikes int
}

// TestCredentialNow probes one credential on demand and reports what the upstream
// answered.
//
// It deliberately does not touch scheduling state. A manual test is a question
// ("can this serve right now?"), and letting it write strikes or cooldowns would
// give a button press the power to auto-disable a credential: a credential
// answering 429 three times while an operator pokes at it would cross the
// deterministic threshold through nothing but repeated clicking. The scheduled
// loop stays the only writer, which is what keeps that threshold meaningful.
//
// model overrides the deep-probe model for this call; empty uses the configured
// one. A provider that supports a real generation probe always gets one here,
// regardless of the sampling percentage: sampling exists to bound the cost of a
// scheduled sweep, and an operator asking about one credential is not that.
func (m *Manager) TestCredentialNow(ctx context.Context, authID string, model string) (CredentialTestResult, error) {
	if m == nil {
		return CredentialTestResult{}, ErrCredentialTestUnavailable
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return CredentialTestResult{}, ErrCredentialTestUnavailable
	}
	auth, ok := m.GetByID(authID)
	if !ok || auth == nil {
		return CredentialTestResult{}, ErrCredentialTestNotFound
	}

	result := CredentialTestResult{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Label:    auth.Label,
	}
	if state := auth.SelfTestState(); !state.IsZero() {
		result.Verdict = string(state.Verdict)
		result.Strikes = state.Strikes
	}

	provider := strings.TrimSpace(auth.Provider)
	tester, ok := m.credentialSelfTester(provider)
	if !ok {
		// Not every provider has a check endpoint that is safe to call; saying so
		// is better than guessing at a probe.
		return result, nil
	}

	timeout := m.selfTestOptions().Timeout
	if timeout <= 0 {
		timeout = DefaultSelfTestOptions().Timeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	startedAt := time.Now()
	probeResult, errProbe := tester.SelfTestCredential(probeCtx, auth)
	if errProbe != nil {
		// An executor that attaches a status is reporting a rejection it already
		// got upstream (a refused refresh token, for example), which is a real
		// answer rather than a failure to ask.
		if status, ok := selfTestErrorStatus(errProbe); ok {
			result.Probed = true
			result.StatusCode = status
			result.Message = errProbe.Error()
			result.Duration = time.Since(startedAt)
			result.Healthy = status >= http.StatusOK && status < http.StatusMultipleChoices
			result.Tier = SelfTestTierAuthorization
			return result, nil
		}
		result.Err = errProbe
		result.Duration = time.Since(startedAt)
		return result, nil
	}
	if probeResult == nil {
		return result, nil
	}
	result.Probed = true

	// The cheap probe only speaks for authorization. Whenever the provider can
	// make a real call, follow up with one, because that is the question the
	// dispatcher actually cares about: a credential whose quota is spent passes
	// the cheap probe and then fails every real request.
	if probeResult.StatusCode >= http.StatusOK && probeResult.StatusCode < http.StatusMultipleChoices {
		if deep, ok := m.credentialDeepProbe(probeCtx, tester, auth, model); ok {
			result.StatusCode = deep.StatusCode
			result.Message = credentialSelfTestMessage(deep)
			result.ForbiddenType = deep.ForbiddenType
			result.ValidationURL = deep.ValidationURL
			result.Tier = SelfTestTierGeneration
			result.Healthy = deep.StatusCode >= http.StatusOK && deep.StatusCode < http.StatusMultipleChoices
			result.Duration = time.Since(startedAt)
			return result, nil
		}
	}

	result.StatusCode = probeResult.StatusCode
	result.Message = credentialSelfTestMessage(*probeResult)
	result.ForbiddenType = probeResult.ForbiddenType
	result.ValidationURL = probeResult.ValidationURL
	tier := probeResult.Tier
	if tier == 0 {
		tier = SelfTestTierAuthorization
	}
	result.Tier = tier
	result.Healthy = probeResult.StatusCode >= http.StatusOK && probeResult.StatusCode < http.StatusMultipleChoices
	result.Duration = time.Since(startedAt)
	return result, nil
}

// credentialDeepProbe runs the real generation probe for a manual test, reporting
// whether it produced a result. It mirrors the loop's deep probe minus the
// sampling decision: an operator asking about one named credential is not a
// scheduled sweep, so the percentage does not apply.
func (m *Manager) credentialDeepProbe(ctx context.Context, tester CredentialSelfTester, auth *Auth, model string) (CredentialSelfTestResult, bool) {
	generator, ok := tester.(CredentialGenerationSelfTester)
	if !ok {
		return CredentialSelfTestResult{}, false
	}
	if strings.TrimSpace(model) == "" {
		model = m.selfTestOptions().DeepProbeModel
	}
	if strings.TrimSpace(model) == "" {
		return CredentialSelfTestResult{}, false
	}
	result, errProbe := generator.SelfTestCredentialGeneration(ctx, auth, model)
	if errProbe != nil {
		log.Debugf("credential self-test: manual deep probe of %s %s failed to run: %v", auth.Provider, auth.ID, errProbe)
		return CredentialSelfTestResult{}, false
	}
	if result == nil {
		return CredentialSelfTestResult{}, false
	}
	return *result, true
}
