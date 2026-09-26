package auth

import (
	"context"
	"testing"
)

func resetCredentialInvalidHits() {
	credentialInvalidHits.Range(func(key, _ any) bool {
		credentialInvalidHits.Delete(key)
		return true
	})
}

func registerCredentialInvalidAuth(t *testing.T, m *Manager, id string) {
	t.Helper()
	if _, errRegister := m.Register(context.Background(), &Auth{ID: id, Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
}

func credentialInvalidResult(authID, model string) Result {
	return Result{
		AuthID:   authID,
		Provider: "antigravity",
		Model:    model,
		Success:  false,
		Error: &Error{
			Code:       credentialInvalidErrorCode,
			Message:    "Verify your account to continue.",
			HTTPStatus: 403,
		},
	}
}

func TestManager_MarkResult_DisablesCredentialAfterThreshold(t *testing.T) {
	prev := disableCredentialInvalidAuths.Load()
	disableCredentialInvalidAuths.Store(true)
	t.Cleanup(func() {
		disableCredentialInvalidAuths.Store(prev)
		resetCredentialInvalidHits()
	})
	resetCredentialInvalidHits()

	m := NewManager(nil, nil, nil)
	registerCredentialInvalidAuth(t, m, "auth-invalid")
	const model = "claude-sonnet-4-5"

	// First failure only counts; a single hit can still be an upstream hiccup.
	m.MarkResult(context.Background(), credentialInvalidResult("auth-invalid", model))
	if auth, _ := m.GetByID("auth-invalid"); auth == nil || auth.Disabled {
		t.Fatalf("auth must stay enabled after a single failure")
	}

	m.MarkResult(context.Background(), credentialInvalidResult("auth-invalid", model))
	auth, ok := m.GetByID("auth-invalid")
	if !ok || auth == nil {
		t.Fatalf("expected auth to be present")
	}
	if !auth.Disabled {
		t.Fatal("auth must be disabled once the threshold is reached")
	}
	if auth.Status != StatusDisabled {
		t.Fatalf("status = %q, want %q", auth.Status, StatusDisabled)
	}
	// The upstream message has to survive so the operator can see what to fix.
	if auth.StatusMessage == "" {
		t.Fatal("expected a status message explaining the disable")
	}
	if disabled, _ := auth.Metadata["disabled"].(bool); !disabled {
		t.Fatal("expected metadata.disabled so the state survives a restart")
	}
}

func TestManager_MarkResult_KeepsCredentialWhenSwitchIsOff(t *testing.T) {
	prev := disableCredentialInvalidAuths.Load()
	disableCredentialInvalidAuths.Store(false)
	t.Cleanup(func() {
		disableCredentialInvalidAuths.Store(prev)
		resetCredentialInvalidHits()
	})
	resetCredentialInvalidHits()

	m := NewManager(nil, nil, nil)
	registerCredentialInvalidAuth(t, m, "auth-kept")
	const model = "claude-sonnet-4-5"

	for range credentialInvalidHitsBeforeDisable + 2 {
		m.MarkResult(context.Background(), credentialInvalidResult("auth-kept", model))
	}
	if auth, _ := m.GetByID("auth-kept"); auth == nil || auth.Disabled {
		t.Fatal("auth must not be disabled while the switch is off")
	}
}

func TestManager_MarkResult_SuccessResetsCredentialInvalidHits(t *testing.T) {
	prev := disableCredentialInvalidAuths.Load()
	disableCredentialInvalidAuths.Store(true)
	t.Cleanup(func() {
		disableCredentialInvalidAuths.Store(prev)
		resetCredentialInvalidHits()
	})
	resetCredentialInvalidHits()

	m := NewManager(nil, nil, nil)
	registerCredentialInvalidAuth(t, m, "auth-flaky")
	const model = "claude-sonnet-4-5"

	m.MarkResult(context.Background(), credentialInvalidResult("auth-flaky", model))
	// A recovery between failures means the credential is not permanently broken,
	// so the counter must not carry over and disable it later.
	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-flaky",
		Provider: "antigravity",
		Model:    model,
		Success:  true,
	})
	m.MarkResult(context.Background(), credentialInvalidResult("auth-flaky", model))

	if auth, _ := m.GetByID("auth-flaky"); auth == nil || auth.Disabled {
		t.Fatal("a recovered credential must not be disabled by stale hits")
	}
}

func TestIsCredentialInvalidResultError(t *testing.T) {
	if isCredentialInvalidResultError(nil) {
		t.Fatal("nil error must not be credential_invalid")
	}
	if isCredentialInvalidResultError(&Error{Code: requestScopedErrorCode}) {
		t.Fatal("request_scoped must not be credential_invalid")
	}
	if !isCredentialInvalidResultError(&Error{Code: credentialInvalidErrorCode}) {
		t.Fatal("credential_invalid code must be recognised")
	}
}
