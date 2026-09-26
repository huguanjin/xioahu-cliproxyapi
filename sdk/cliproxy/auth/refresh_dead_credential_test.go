package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// deadRefreshTokenError mirrors the executor's statusErr shape: an error whose
// type reports that the credential cannot recover without operator action.
type deadRefreshTokenError struct{}

func (deadRefreshTokenError) Error() string { return "invalid_grant" }

func (deadRefreshTokenError) StatusCode() int { return http.StatusBadRequest }

func (deadRefreshTokenError) CredentialNeedsAction() bool { return true }

// transientRefreshError is the same error without the credential-action flag.
type transientRefreshError struct{}

func (transientRefreshError) Error() string { return "status 503" }

func (transientRefreshError) StatusCode() int { return http.StatusServiceUnavailable }

// refreshFailureExecutor is a ProviderExecutor that always fails Refresh with a
// fixed error, so the conductor's refresh path can be exercised directly.
type refreshFailureExecutor struct {
	err error
}

func (refreshFailureExecutor) Identifier() string { return "antigravity" }

func (refreshFailureExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (refreshFailureExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e refreshFailureExecutor) Refresh(context.Context, *Auth) (*Auth, error) { return nil, e.err }

func (refreshFailureExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (refreshFailureExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func TestManager_RefreshAuthForRequest_DisablesDeadCredential(t *testing.T) {
	prev := disableCredentialInvalidAuths.Load()
	disableCredentialInvalidAuths.Store(true)
	t.Cleanup(func() { disableCredentialInvalidAuths.Store(prev) })

	m := NewManager(nil, nil, nil)
	if _, errRegister := m.Register(context.Background(), &Auth{ID: "auth-dead", Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	m.executors["antigravity"] = refreshFailureExecutor{err: deadRefreshTokenError{}}

	// The refresh failure must surface to the caller...
	if _, errRefresh := m.refreshAuthForRequest(context.Background(), "auth-dead", ""); errRefresh == nil {
		t.Fatal("expected the refresh error to be returned")
	}

	// ...while the credential is taken out of rotation on the first failure,
	// because a rejected refresh token is deterministic rather than flaky.
	auth, ok := m.GetByID("auth-dead")
	if !ok || auth == nil {
		t.Fatal("expected auth to be present")
	}
	if !auth.Disabled {
		t.Fatal("a dead refresh token must disable the credential immediately")
	}
	if auth.Status != StatusDisabled {
		t.Fatalf("status = %q, want %q", auth.Status, StatusDisabled)
	}
	if auth.StatusMessage == "" {
		t.Fatal("expected a status message explaining the disable")
	}
	if disabled, _ := auth.Metadata["disabled"].(bool); !disabled {
		t.Fatal("expected metadata.disabled so the state survives a restart")
	}
}

func TestManager_RefreshAuthForRequest_KeepsTransientFailure(t *testing.T) {
	prev := disableCredentialInvalidAuths.Load()
	disableCredentialInvalidAuths.Store(true)
	t.Cleanup(func() { disableCredentialInvalidAuths.Store(prev) })

	m := NewManager(nil, nil, nil)
	if _, errRegister := m.Register(context.Background(), &Auth{ID: "auth-flaky", Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	m.executors["antigravity"] = refreshFailureExecutor{err: transientRefreshError{}}

	if _, errRefresh := m.refreshAuthForRequest(context.Background(), "auth-flaky", ""); errRefresh == nil {
		t.Fatal("expected the refresh error to be returned")
	}

	auth, ok := m.GetByID("auth-flaky")
	if !ok || auth == nil {
		t.Fatal("expected auth to be present")
	}
	if auth.Disabled {
		t.Fatal("a transient refresh failure must not disable the credential")
	}
}

func TestManager_RefreshAuthForRequest_DeadCredentialKeptWhenSwitchOff(t *testing.T) {
	prev := disableCredentialInvalidAuths.Load()
	disableCredentialInvalidAuths.Store(false)
	t.Cleanup(func() { disableCredentialInvalidAuths.Store(prev) })

	m := NewManager(nil, nil, nil)
	if _, errRegister := m.Register(context.Background(), &Auth{ID: "auth-opted-out", Provider: "antigravity"}); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}
	m.executors["antigravity"] = refreshFailureExecutor{err: deadRefreshTokenError{}}

	if _, errRefresh := m.refreshAuthForRequest(context.Background(), "auth-opted-out", ""); errRefresh == nil {
		t.Fatal("expected the refresh error to be returned")
	}

	// Taking a credential out of rotation stays operator-visible, so the
	// disable-unauthorized-auths switch has to gate this path too.
	auth, ok := m.GetByID("auth-opted-out")
	if !ok || auth == nil {
		t.Fatal("expected auth to be present")
	}
	if auth.Disabled {
		t.Fatal("the credential must stay enabled while the switch is off")
	}
}
