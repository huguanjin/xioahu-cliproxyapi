package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// selfTestStubExecutor answers a scripted sequence of probe results so a manual
// test can be exercised without an upstream.
type selfTestStubExecutor struct {
	id string
	// probeStatus is returned by the cheap probe.
	probeStatus int
	probeBody   string
	probeErr    error
	// genStatus is returned by the deep probe, when the executor supports one.
	genStatus   int
	genErr      error
	supportsGen bool
	probeCalls  int
	genCalls    int
}

func (s *selfTestStubExecutor) Identifier() string { return s.id }
func (s *selfTestStubExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (s *selfTestStubExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (s *selfTestStubExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }
func (s *selfTestStubExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (s *selfTestStubExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (s *selfTestStubExecutor) SelfTestCredential(context.Context, *Auth) (*CredentialSelfTestResult, error) {
	s.probeCalls++
	if s.probeErr != nil {
		return nil, s.probeErr
	}
	return &CredentialSelfTestResult{
		StatusCode: s.probeStatus,
		Message:    s.probeBody,
		Tier:       SelfTestTierAuthorization,
	}, nil
}

func (s *selfTestStubExecutor) SelfTestCredentialGeneration(context.Context, *Auth, string) (*CredentialSelfTestResult, error) {
	s.genCalls++
	if s.genErr != nil {
		return nil, s.genErr
	}
	return &CredentialSelfTestResult{
		StatusCode: s.genStatus,
		Tier:       SelfTestTierGeneration,
	}, nil
}

// A manual test is a question, not a scheduling decision. Three clicks on a
// credential that happens to be rate limited must not walk it up the strikes
// ladder toward auto-disable, which is the exact false positive the threshold
// exists to avoid.
func TestCredentialTestDoesNotTouchSchedulingState(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	exec := &selfTestStubExecutor{id: "antigravity", probeStatus: http.StatusTooManyRequests}
	manager.RegisterExecutor(exec)
	auth := &Auth{ID: "a", Provider: "antigravity", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	for i := 0; i < 5; i++ {
		result, errTest := manager.TestCredentialNow(context.Background(), "a", "")
		if errTest != nil {
			t.Fatalf("TestCredentialNow() error = %v", errTest)
		}
		if !result.Probed {
			t.Fatal("expected the probe to run")
		}
		if result.Healthy {
			t.Fatal("a 429 must not be reported as healthy")
		}
	}

	updated, ok := manager.GetByID("a")
	if !ok || updated == nil {
		t.Fatal("expected the auth to still be registered")
	}
	state := updated.SelfTestState()
	if state.Strikes != 0 {
		t.Fatalf("a manual test must not add strikes, got %d", state.Strikes)
	}
	if state.Verdict != SelfTestVerdictUnknown {
		t.Fatalf("a manual test must not record a verdict, got %q", state.Verdict)
	}
	if state.AutoDisabled || updated.Disabled {
		t.Fatal("a manual test must never auto-disable a credential")
	}
	if !updated.NextRetryAfter.IsZero() {
		t.Fatalf("a manual test must not set a cooldown, got %v", updated.NextRetryAfter)
	}
	if updated.Quota.PaymentBackoffLevel != 0 {
		t.Fatalf("a manual test must not advance the payment ladder, got %d", updated.Quota.PaymentBackoffLevel)
	}
}

// The deep probe is what answers "can this serve right now". A manual test must
// run it whenever the provider supports one, regardless of the sampling
// percentage that bounds a scheduled sweep.
func TestCredentialTestAlwaysRunsDeepProbe(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	exec := &selfTestStubExecutor{id: "antigravity", probeStatus: http.StatusOK, genStatus: http.StatusTooManyRequests}
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID: "a", Provider: "antigravity", Status: StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	// A sampling percentage of zero disables deep probing on the schedule; a
	// manual test must ignore it.
	manager.PublishSelfTestOptions(SelfTestOptions{DeepProbeSamplePercent: 0, DeepProbeEnabled: true})

	result, errTest := manager.TestCredentialNow(context.Background(), "a", "")
	if errTest != nil {
		t.Fatalf("TestCredentialNow() error = %v", errTest)
	}
	if exec.genCalls != 1 {
		t.Fatalf("expected exactly one deep probe, got %d", exec.genCalls)
	}
	if result.Tier != SelfTestTierGeneration {
		t.Fatalf("tier = %d, want the generation tier", result.Tier)
	}
	if result.Healthy {
		t.Fatal("a spent quota must not be reported as healthy")
	}
	if result.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", result.StatusCode)
	}
}

// A provider with no check endpoint must report that honestly rather than
// answering "healthy" about a credential nothing was asked of.
func TestCredentialTestReportsUnprobeableProvider(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(stubExecutor{id: "plain"})
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID: "a", Provider: "plain", Status: StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	result, errTest := manager.TestCredentialNow(context.Background(), "a", "")
	if errTest != nil {
		t.Fatalf("TestCredentialNow() error = %v", errTest)
	}
	if result.Probed {
		t.Fatal("a provider without a check endpoint must report probed=false")
	}
	if result.Healthy {
		t.Fatal("an unprobed credential must never be reported as healthy")
	}
}

// A probe that could not run carries no verdict. Reporting it as a rejection
// would blame the credential for our own network problem.
func TestCredentialTestSurfacesTransportFailure(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	exec := &selfTestStubExecutor{id: "antigravity", probeErr: errors.New("dial tcp: i/o timeout")}
	manager.RegisterExecutor(exec)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID: "a", Provider: "antigravity", Status: StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	result, errTest := manager.TestCredentialNow(context.Background(), "a", "")
	if errTest != nil {
		t.Fatalf("TestCredentialNow() error = %v", errTest)
	}
	if result.Probed {
		t.Fatal("a transport failure is not a probe that ran")
	}
	if result.Healthy {
		t.Fatal("a transport failure must not read as healthy")
	}
	if result.Err == nil {
		t.Fatal("the transport failure must be reported so the UI can distinguish it")
	}
}

// An unknown credential is a caller error, not a probe result.
func TestCredentialTestUnknownAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, errTest := manager.TestCredentialNow(context.Background(), "missing", ""); !errors.Is(errTest, ErrCredentialTestNotFound) {
		t.Fatalf("error = %v, want ErrCredentialTestNotFound", errTest)
	}
	if _, errTest := manager.TestCredentialNow(context.Background(), "  ", ""); !errors.Is(errTest, ErrCredentialTestUnavailable) {
		t.Fatalf("error = %v, want ErrCredentialTestUnavailable", errTest)
	}
}

// The reported verdict describes the loop's state; it is not this call's doing.
func TestCredentialTestReportsExistingVerdict(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	exec := &selfTestStubExecutor{id: "antigravity", probeStatus: http.StatusOK, genStatus: http.StatusOK}
	manager.RegisterExecutor(exec)
	auth := &Auth{ID: "a", Provider: "antigravity", Status: StatusActive}
	auth.SetSelfTestState(SelfTestState{Strikes: 2, Verdict: SelfTestVerdictCooling})
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	result, errTest := manager.TestCredentialNow(context.Background(), "a", "")
	if errTest != nil {
		t.Fatalf("TestCredentialNow() error = %v", errTest)
	}
	if !result.Healthy {
		t.Fatal("expected the credential to answer healthy")
	}
	if result.Verdict != string(SelfTestVerdictCooling) || result.Strikes != 2 {
		t.Fatalf("expected the existing verdict to be reported, got %q strikes=%d", result.Verdict, result.Strikes)
	}
	// Reporting the verdict must not have changed it.
	after, ok := manager.GetByID("a")
	if !ok || after == nil {
		t.Fatal("expected the auth to still be registered")
	}
	if state := after.SelfTestState(); state.Strikes != 2 || state.Verdict != SelfTestVerdictCooling {
		t.Fatalf("the loop's state must survive a manual test, got %+v", state)
	}
}

// A revoked refresh token is an OAuth 400, which the status-code switch alone
// would class as transient and excuse. That left a permanently dead credential in
// the pool, retried on every sweep forever — the exact outcome the probe exists to
// prevent. The executor says so out of band, and that signal must reach the kind.
func TestApplyCredentialSelfTestTreatsRevokedAsCooling(t *testing.T) {
	withQuotaCooldownEnabled(t)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "revoked", Provider: "antigravity", Metadata: map[string]any{"type": "antigravity"}}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	failure := manager.ApplyCredentialSelfTest(context.Background(), CredentialSelfTestResult{
		AuthID:            auth.ID,
		Provider:          "antigravity",
		StatusCode:        http.StatusBadRequest,
		Message:           `{"error":"invalid_grant"}`,
		ProbeError:        true,
		CredentialRevoked: true,
	})
	if failure.Kind != SelfTestFailureRevoked {
		t.Fatalf("kind = %q, want %q", failure.Kind, SelfTestFailureRevoked)
	}
	updated, _ := manager.GetByID(auth.ID)
	if updated == nil {
		t.Fatal("expected the auth to still be registered")
	}
	state := updated.SelfTestState()
	if state.Strikes != 1 {
		t.Fatalf("a revoked credential must earn a strike, got %d", state.Strikes)
	}
	if state.Verdict != SelfTestVerdictCooling {
		t.Fatalf("verdict = %q, want cooling", state.Verdict)
	}
	if updated.NextRetryAfter.IsZero() || !updated.NextRetryAfter.After(time.Now()) {
		t.Fatalf("a revoked credential must be cooled down, got %v", updated.NextRetryAfter)
	}
}

// The operator asked to watch this class before the pool drops it. Cooling keeps
// it out of dispatch, which is the property that matters; auto-disable is the
// irreversible half and must not happen here.
func TestRevokedCredentialIsNeverAutoDisabled(t *testing.T) {
	withQuotaCooldownEnabled(t)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "revoked", Provider: "antigravity", Metadata: map[string]any{"type": "antigravity"}}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	// Auto-disable is on, so only the revoked rule can be what stops it.
	manager.PublishSelfTestOptions(SelfTestOptions{
		AutoDisableOnThreshold:        true,
		DeterministicFailureThreshold: 3,
	})

	for i := 0; i < 6; i++ {
		manager.ApplyCredentialSelfTest(context.Background(), CredentialSelfTestResult{
			AuthID:            auth.ID,
			StatusCode:        http.StatusBadRequest,
			Message:           `{"error":"invalid_grant"}`,
			ProbeError:        true,
			CredentialRevoked: true,
		})
	}

	updated, _ := manager.GetByID(auth.ID)
	if updated == nil {
		t.Fatal("expected the auth to still be registered")
	}
	if updated.Disabled {
		t.Fatal("a revoked credential must never be auto-disabled: the operator asked to observe it first")
	}
	state := updated.SelfTestState()
	if state.AutoDisabled {
		t.Fatal("the auto-disabled flag must stay clear for a revoked credential")
	}
	if state.Strikes < 3 {
		t.Fatalf("strikes should still accrue, got %d", state.Strikes)
	}
	// It must still be out of dispatch, which the cooldown achieves on its own.
	if updated.NextRetryAfter.IsZero() {
		t.Fatal("a revoked credential must stay cooled down even without auto-disable")
	}
}

// An ordinary deterministic failure must still auto-disable, so the revoked rule
// is a carve-out rather than a hole in the threshold.
func TestDeterministicFailureStillAutoDisables(t *testing.T) {
	withQuotaCooldownEnabled(t)
	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "banned", Provider: "antigravity", Metadata: map[string]any{"type": "antigravity"}}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	manager.PublishSelfTestOptions(SelfTestOptions{
		AutoDisableOnThreshold:        true,
		DeterministicFailureThreshold: 3,
	})

	for i := 0; i < 3; i++ {
		manager.ApplyCredentialSelfTest(context.Background(), CredentialSelfTestResult{
			AuthID:     auth.ID,
			StatusCode: http.StatusForbidden,
			Message:    `{"error":{"message":"violation of Terms of Service"}}`,
		})
	}

	updated, _ := manager.GetByID(auth.ID)
	if updated == nil {
		t.Fatal("expected the auth to still be registered")
	}
	if !updated.Disabled {
		t.Fatal("a credentialed ban must still auto-disable at the threshold")
	}
}

// The revoked marker must survive the error path, since that is how the executor
// reports it: an error carrying CredentialNeedsAction, not a result.
func TestSelfTestErrorDetailsCarriesRevokedMarker(t *testing.T) {
	status, revoked, ok := selfTestErrorDetails(revokedProbeError{})
	if !ok || !revoked {
		t.Fatalf("ok=%v revoked=%v, want both true", ok, revoked)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}

	// A plain rejection carries no marker, and must not be read as revoked.
	_, revoked, ok = selfTestErrorDetails(plainProbeError{})
	if !ok || revoked {
		t.Fatalf("ok=%v revoked=%v, want ok=true revoked=false", ok, revoked)
	}

	// A 5xx is upstream trouble and carries no verdict at all.
	if _, _, ok := selfTestErrorDetails(serverProbeError{}); ok {
		t.Fatal("a 5xx must not be treated as a credential verdict")
	}
}

type revokedProbeError struct{}

func (revokedProbeError) Error() string               { return "invalid_grant" }
func (revokedProbeError) StatusCode() int             { return http.StatusBadRequest }
func (revokedProbeError) CredentialNeedsAction() bool { return true }

type plainProbeError struct{}

func (plainProbeError) Error() string   { return "rejected" }
func (plainProbeError) StatusCode() int { return http.StatusBadRequest }

type serverProbeError struct{}

func (serverProbeError) Error() string   { return "upstream down" }
func (serverProbeError) StatusCode() int { return http.StatusServiceUnavailable }
