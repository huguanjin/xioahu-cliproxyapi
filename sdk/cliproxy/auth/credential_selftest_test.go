package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// A probe rejection must reach the same payment ladder a live 403 reaches, so a
// credential that stops working drains out of rotation without waiting for a
// user request to discover it.
func TestApplyCredentialSelfTestDrivesPaymentCooldown(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-selftest-403",
		Provider: "antigravity",
		Metadata: map[string]any{"type": "antigravity"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	manager.ApplyCredentialSelfTest(context.Background(), CredentialSelfTestResult{
		AuthID:     auth.ID,
		Provider:   "antigravity",
		StatusCode: http.StatusForbidden,
		Message:    `{"error":{"status":"PERMISSION_DENIED"}}`,
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected the auth to still be registered")
	}
	if updated.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected the payment ladder to advance to 1, got %d", updated.Quota.PaymentBackoffLevel)
	}
	if updated.NextRetryAfter.IsZero() || !updated.NextRetryAfter.After(time.Now()) {
		t.Fatalf("expected a future retry deadline, got %v", updated.NextRetryAfter)
	}
	if !updated.Unavailable {
		t.Fatalf("expected the credential to be marked unavailable while cooling")
	}
	// The probe reports on the credential, not on one model, so nothing may
	// land in per-model state.
	if len(updated.ModelStates) != 0 {
		t.Fatalf("expected no per-model state from a credential-level probe, got %d entries", len(updated.ModelStates))
	}
	// A probe must never disable a credential; that path stays reserved for
	// failures observed while serving real traffic.
	if updated.Disabled {
		t.Fatalf("a self-test rejection must not disable the credential")
	}
}

// A healthy probe answers only that a cheap endpoint accepted the credential.
// It says nothing about the state live traffic left behind, so it must not
// clear an existing cooldown or its ladder.
func TestApplyCredentialSelfTestLeavesStateOnSuccess(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-selftest-ok",
		Provider: "antigravity",
		Quota: QuotaState{
			Exceeded:            true,
			Reason:              "payment_required",
			NextRecoverAt:       time.Now().Add(time.Hour),
			PaymentBackoffLevel: 3,
		},
		Unavailable:    true,
		NextRetryAfter: time.Now().Add(time.Hour),
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	manager.ApplyCredentialSelfTest(context.Background(), CredentialSelfTestResult{
		AuthID:     auth.ID,
		Provider:   "antigravity",
		StatusCode: http.StatusOK,
	})

	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatalf("expected the auth to still be registered")
	}
	if updated.Quota.PaymentBackoffLevel != 3 {
		t.Fatalf("expected the payment ladder to survive a healthy probe at 3, got %d", updated.Quota.PaymentBackoffLevel)
	}
	if !updated.Quota.Exceeded {
		t.Fatalf("expected the existing cooldown to survive a healthy probe")
	}
}

// Transient upstream trouble is not a verdict on the credential, so the probe
// must stay silent rather than cool a healthy credential down.
func TestApplyCredentialSelfTestIgnoresTransientStatus(t *testing.T) {
	withQuotaCooldownEnabled(t)

	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusRequestTimeout, http.StatusServiceUnavailable} {
		manager := NewManager(nil, nil, nil)
		auth := &Auth{
			ID:       "auth-selftest-transient",
			Provider: "antigravity",
		}
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("Register returned error: %v", errRegister)
		}

		manager.ApplyCredentialSelfTest(context.Background(), CredentialSelfTestResult{
			AuthID:     auth.ID,
			StatusCode: status,
		})

		updated, _ := manager.GetByID(auth.ID)
		if updated == nil {
			t.Fatalf("expected the auth to still be registered for status %d", status)
		}
		if updated.Quota.PaymentBackoffLevel != 0 || !updated.NextRetryAfter.IsZero() {
			t.Fatalf("status %d must not cool the credential, got level %d until %v",
				status, updated.Quota.PaymentBackoffLevel, updated.NextRetryAfter)
		}
	}
}

// A probe that never reached the upstream carries no verdict either.
func TestApplyCredentialSelfTestIgnoresMissingStatus(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-selftest-nostatus", Provider: "antigravity"}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	manager.ApplyCredentialSelfTest(context.Background(), CredentialSelfTestResult{AuthID: auth.ID})
	updated, _ := manager.GetByID(auth.ID)
	if updated == nil {
		t.Fatalf("expected the auth to still be registered")
	}
	if updated.Quota.PaymentBackoffLevel != 0 || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("a transport-level probe failure must not cool the credential")
	}
}

// Probing a credential that is already parked invites only a duplicate
// rejection, and a disabled credential is out of rotation by operator intent.
func TestCredentialSelfTestEligible(t *testing.T) {
	future := time.Now().Add(time.Hour)
	cases := []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "healthy", auth: &Auth{ID: "a", Provider: "antigravity"}, want: true},
		{name: "nil", auth: nil, want: false},
		{name: "no id", auth: &Auth{Provider: "antigravity"}, want: false},
		{name: "disabled", auth: &Auth{ID: "a", Disabled: true}, want: false},
		{name: "unavailable", auth: &Auth{ID: "a", Unavailable: true}, want: false},
		{name: "quota exceeded", auth: &Auth{ID: "a", Quota: QuotaState{Exceeded: true}}, want: false},
		{name: "retry pending", auth: &Auth{ID: "a", NextRetryAfter: future}, want: false},
		{name: "recover pending", auth: &Auth{ID: "a", Quota: QuotaState{NextRecoverAt: future}}, want: false},
		{
			name: "window expired",
			auth: &Auth{ID: "a", NextRetryAfter: time.Now().Add(-time.Minute), Quota: QuotaState{NextRecoverAt: time.Now().Add(-time.Minute)}},
			want: true,
		},
	}
	for _, tc := range cases {
		if got := selfTestEligible(tc.auth); got != tc.want {
			t.Errorf("%s: expected eligible=%v, got %v", tc.name, tc.want, got)
		}
	}
}

// The loop must not re-probe the same credential on every tick; the per-
// credential period is what keeps the probe from becoming upstream load.
func TestCredentialSelfTestLoopCandidatesRespectPeriod(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	for _, id := range []string{"a", "b"} {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
			ID:       id,
			Provider: "antigravity",
			Status:   StatusActive,
		}); errRegister != nil {
			t.Fatalf("Register returned error: %v", errRegister)
		}
	}
	loop := &credentialSelfTestLoop{
		manager: manager,
		options: SelfTestOptions{
			Interval:            time.Minute,
			PerCredentialPeriod: time.Hour,
			Timeout:             time.Second,
			Concurrency:         2,
		},
		lastProbed: make(map[string]time.Time),
	}

	first, skipped := loop.candidates(time.Now(), true)
	if len(first) != 2 {
		t.Fatalf("expected both credentials to be due on the first tick, got %d (skipped %d)", len(first), skipped)
	}
	if again, _ := loop.candidates(time.Now(), true); len(again) != 0 {
		t.Fatalf("expected no credential to be due again within the period, got %d", len(again))
	}

	// Once the period elapses both become due again.
	later := time.Now().Add(2 * time.Hour)
	if after, _ := loop.candidates(later, true); len(after) != 2 {
		t.Fatalf("expected both credentials to be due after the period, got %d", len(after))
	}
}

// A manual run bypasses the per-credential period, so pressing the button twice
// in a row still probes everything.
func TestCredentialSelfTestLoopCandidatesBypassesPeriodOnManualRun(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "a",
		Provider: "antigravity",
		Status:   StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	loop := &credentialSelfTestLoop{
		manager: manager,
		options: SelfTestOptions{
			Interval:            time.Minute,
			PerCredentialPeriod: time.Hour,
			Timeout:             time.Second,
			Concurrency:         2,
		},
		lastProbed: make(map[string]time.Time, 0),
	}
	now := time.Now()
	loop.lastProbed["a"] = now

	due, skipped := loop.candidates(now, false)
	if len(due) != 1 {
		t.Fatalf("expected the period to be ignored on a manual run, got %d due (skipped %d)", len(due), skipped)
	}
}

// Providers without a probe must be skipped rather than guessed at.
func TestCredentialSelfTesterRequiresImplementingExecutor(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(stubExecutor{id: "plain"})
	if _, ok := manager.credentialSelfTester("plain"); ok {
		t.Fatalf("an executor without SelfTestCredential must not be treated as a probe target")
	}
	if _, ok := manager.credentialSelfTester("missing"); ok {
		t.Fatalf("an unknown provider must not be treated as a probe target")
	}
}

type stubExecutor struct{ id string }

func (s stubExecutor) Identifier() string { return s.id }
func (s stubExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (s stubExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (s stubExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, nil }
func (s stubExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (s stubExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}
