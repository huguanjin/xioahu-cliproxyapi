package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func paymentResult(authID, model string, status int) Result {
	return Result{
		AuthID:   authID,
		Provider: "codex",
		Model:    model,
		Success:  false,
		Error: &Error{
			Code:       "payment_required",
			Message:    "payment required",
			Retryable:  false,
			HTTPStatus: status,
		},
	}
}

func TestApplyAuthFailureStatePaymentBackoffEscalates(t *testing.T) {
	now := time.Now()
	paymentErr := &Error{Code: "payment_required", Message: "payment required", HTTPStatus: http.StatusForbidden}
	auth := &Auth{ID: "auth-level-payment"}

	applyAuthFailureState(auth, paymentErr, nil, now, false)
	if auth.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected PaymentBackoffLevel 1 after first failure, got %d", auth.Quota.PaymentBackoffLevel)
	}
	if !auth.Quota.NextRecoverAt.Equal(now.Add(paymentBackoffBase)) {
		t.Fatalf("expected first window to close at %v, got %v", now.Add(paymentBackoffBase), auth.Quota.NextRecoverAt)
	}
	if !auth.NextRetryAfter.Equal(auth.Quota.NextRecoverAt) {
		t.Fatalf("expected NextRetryAfter %v to match the quota window %v", auth.NextRetryAfter, auth.Quota.NextRecoverAt)
	}

	// In-window failures reuse the open window and cannot inflate the ladder.
	firstRecover := auth.Quota.NextRecoverAt
	applyAuthFailureState(auth, paymentErr, nil, now.Add(time.Minute), false)
	if auth.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected PaymentBackoffLevel to stay 1 for in-window failure, got %d", auth.Quota.PaymentBackoffLevel)
	}
	if !auth.Quota.NextRecoverAt.Equal(firstRecover) {
		t.Fatalf("expected NextRecoverAt to stay %v for in-window failure, got %v", firstRecover, auth.Quota.NextRecoverAt)
	}

	// A failure after the window expired doubles the cooldown.
	afterWindow := now.Add(paymentBackoffBase + time.Minute)
	applyAuthFailureState(auth, paymentErr, nil, afterWindow, false)
	if auth.Quota.PaymentBackoffLevel != 2 {
		t.Fatalf("expected PaymentBackoffLevel 2 after post-window failure, got %d", auth.Quota.PaymentBackoffLevel)
	}
	if !auth.Quota.NextRecoverAt.Equal(afterWindow.Add(2 * paymentBackoffBase)) {
		t.Fatalf("expected second window to close at %v, got %v", afterWindow.Add(2*paymentBackoffBase), auth.Quota.NextRecoverAt)
	}

	// The ladder is capped so a permanently rejected credential settles at the
	// maximum cooldown instead of growing without bound.
	at := auth.Quota.NextRecoverAt.Add(time.Minute)
	lastWindow := time.Duration(0)
	for i := 0; i < 20; i++ {
		applyAuthFailureState(auth, paymentErr, nil, at, false)
		lastWindow = auth.Quota.NextRecoverAt.Sub(at)
		if lastWindow > paymentBackoffMax {
			t.Fatalf("cooldown %v exceeded the %v cap at step %d", lastWindow, paymentBackoffMax, i)
		}
		if lastWindow == paymentBackoffMax {
			break
		}
		at = auth.Quota.NextRecoverAt.Add(time.Minute)
	}
	if lastWindow != paymentBackoffMax {
		t.Fatalf("expected the ladder to settle at the %v cap, got %v", paymentBackoffMax, lastWindow)
	}
	// Once settled the cooldown stops growing across further failures.
	at = auth.Quota.NextRecoverAt.Add(time.Minute)
	settledLevel := auth.Quota.PaymentBackoffLevel
	applyAuthFailureState(auth, paymentErr, nil, at, false)
	if window := auth.Quota.NextRecoverAt.Sub(at); window != paymentBackoffMax {
		t.Fatalf("expected the capped cooldown to hold, got %v", window)
	}
	if auth.Quota.PaymentBackoffLevel != settledLevel {
		t.Fatalf("expected the level to stop advancing at the cap, got %d", auth.Quota.PaymentBackoffLevel)
	}
}

func TestApplyAuthFailureStatePaymentBackoffDisabledCooling(t *testing.T) {
	now := time.Now()
	paymentErr := &Error{Code: "payment_required", HTTPStatus: http.StatusPaymentRequired}
	auth := &Auth{ID: "auth-payment-disabled-cooling"}

	applyAuthFailureState(auth, paymentErr, nil, now, true)
	if !auth.NextRetryAfter.IsZero() {
		t.Fatalf("expected no retry deadline with cooling disabled, got %v", auth.NextRetryAfter)
	}
	if auth.Quota.PaymentBackoffLevel != 0 {
		t.Fatalf("expected the ladder to stay at 0 with cooling disabled, got %d", auth.Quota.PaymentBackoffLevel)
	}
}

func TestMarkResultPaymentBackoffPerModel(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-model-payment",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	manager.MarkResult(context.Background(), paymentResult(auth.ID, "gpt-5", http.StatusForbidden))
	first, ok := manager.GetByID(auth.ID)
	if !ok || first == nil || first.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after first failure")
	}
	firstState := first.ModelStates["gpt-5"]
	if firstState.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected model PaymentBackoffLevel 1 after first failure, got %d", firstState.Quota.PaymentBackoffLevel)
	}
	if got := time.Until(firstState.NextRetryAfter); got <= 0 || got > paymentBackoffBase+time.Minute {
		t.Fatalf("expected a %v model cooldown, got %v", paymentBackoffBase, got)
	}
	if firstState.Quota.BackoffLevel != 0 {
		t.Fatalf("expected the rate-limit ladder to stay untouched, got %d", firstState.Quota.BackoffLevel)
	}
	// The per-model state is mirrored onto the aggregated auth view so the
	// scheduler and error events see the same cooldown reason.
	if first.Quota.PaymentBackoffLevel != 1 || first.Quota.Reason != "payment_required" {
		t.Fatalf("expected aggregated payment state, got level %d reason %q", first.Quota.PaymentBackoffLevel, first.Quota.Reason)
	}

	// An in-flight 403 lands while the first window is still open.
	manager.MarkResult(context.Background(), paymentResult(auth.ID, "gpt-5", http.StatusForbidden))
	second, ok := manager.GetByID(auth.ID)
	if !ok || second == nil || second.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after second failure")
	}
	secondState := second.ModelStates["gpt-5"]
	if secondState.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected model PaymentBackoffLevel to stay 1 in-window, got %d", secondState.Quota.PaymentBackoffLevel)
	}
	if !secondState.NextRetryAfter.Equal(firstState.NextRetryAfter) {
		t.Fatalf("expected NextRetryAfter to stay %v in-window, got %v", firstState.NextRetryAfter, secondState.NextRetryAfter)
	}
}

// A 403 and a 429 must not contaminate each other's ladder: each failure only
// advances its own level, so a credential that alternates between the two sees
// both cooldowns escalate from where they left off rather than from a shared one.
func TestPaymentBackoffDoesNotShareRateLimitLadder(t *testing.T) {
	now := time.Now()
	auth := &Auth{
		ID: "auth-ladder-separation",
		Quota: QuotaState{
			Exceeded:      true,
			Reason:        "quota",
			NextRecoverAt: now.Add(-time.Second),
			BackoffLevel:  3,
		},
	}

	applyAuthFailureState(auth, &Error{Code: "payment_required", HTTPStatus: http.StatusForbidden}, nil, now, false)
	if auth.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected the payment ladder to start at 1, got %d", auth.Quota.PaymentBackoffLevel)
	}
	if auth.Quota.BackoffLevel != 3 {
		t.Fatalf("expected the rate-limit ladder to stay at 3, got %d", auth.Quota.BackoffLevel)
	}

	// The next rate-limit failure resumes the rate-limit ladder where it stopped.
	auth.Quota.NextRecoverAt = now.Add(-time.Second)
	applyAuthFailureState(auth, &Error{Code: "rate_limit", HTTPStatus: http.StatusTooManyRequests}, nil, now, false)
	if auth.Quota.BackoffLevel != 4 {
		t.Fatalf("expected the rate-limit ladder to resume at 4, got %d", auth.Quota.BackoffLevel)
	}
	if auth.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected the payment ladder to stay at 1, got %d", auth.Quota.PaymentBackoffLevel)
	}
}

// The per-model path must keep the two ladders apart too. The auth-level test
// above only covers applyAuthFailureState; this one covers the ModelStates
// branch, which is the path a real request failure takes.
func TestMarkResultPaymentBackoffKeepsLaddersSeparate(t *testing.T) {
	withQuotaCooldownEnabled(t)

	now := time.Now()
	manager := NewManager(nil, nil, nil)
	auth := &Auth{
		ID:       "auth-model-ladder-separation",
		Provider: "codex",
		Metadata: map[string]any{"type": "codex"},
		ModelStates: map[string]*ModelState{
			"gpt-5": {
				Status:         StatusError,
				Unavailable:    true,
				NextRetryAfter: now.Add(-time.Second),
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: now.Add(-time.Second),
					BackoffLevel:  3,
				},
			},
		},
	}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}

	// A 403 must not erase the progress made through 429s.
	manager.MarkResult(context.Background(), paymentResult(auth.ID, "gpt-5", http.StatusForbidden))
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated == nil || updated.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after 403")
	}
	state := updated.ModelStates["gpt-5"]
	if state.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected the payment ladder to start at 1, got %d", state.Quota.PaymentBackoffLevel)
	}
	if state.Quota.BackoffLevel != 3 {
		t.Fatalf("expected the rate-limit ladder to survive the 403 at 3, got %d", state.Quota.BackoffLevel)
	}

	// And a 429 must not erase the progress made through 403s. Expire the window
	// on the manager's own copy, since GetByID returns a clone.
	expired := now.Add(-time.Second)
	state.NextRetryAfter = expired
	state.Quota.NextRecoverAt = expired
	if _, errUpdate := manager.Update(context.Background(), updated); errUpdate != nil {
		t.Fatalf("Update returned error: %v", errUpdate)
	}
	manager.MarkResult(context.Background(), quotaResult(auth.ID, "gpt-5"))
	resumed, ok := manager.GetByID(auth.ID)
	if !ok || resumed == nil || resumed.ModelStates["gpt-5"] == nil {
		t.Fatalf("expected model state after 429")
	}
	resumedState := resumed.ModelStates["gpt-5"]
	if resumedState.Quota.PaymentBackoffLevel != 1 {
		t.Fatalf("expected the payment ladder to survive the 429 at 1, got %d", resumedState.Quota.PaymentBackoffLevel)
	}
	if resumedState.Quota.BackoffLevel != 4 {
		t.Fatalf("expected the rate-limit ladder to resume at 4, got %d", resumedState.Quota.BackoffLevel)
	}
}

func TestPaymentBackoffResetsOnSuccess(t *testing.T) {
	auth := &Auth{
		ID: "auth-payment-reset",
		Quota: QuotaState{
			Exceeded:            true,
			Reason:              "payment_required",
			NextRecoverAt:       time.Now().Add(time.Hour),
			PaymentBackoffLevel: 4,
		},
	}

	clearAuthStateOnSuccess(auth, time.Now())
	if auth.Quota.PaymentBackoffLevel != 0 {
		t.Fatalf("expected the payment ladder to reset on success, got %d", auth.Quota.PaymentBackoffLevel)
	}
	if auth.Quota.Exceeded || auth.Quota.Reason != "" || !auth.Quota.NextRecoverAt.IsZero() {
		t.Fatalf("expected the quota state to be cleared, got %+v", auth.Quota)
	}
}
