package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// locationUnsupportedBody is the upstream body exactly as Google sends it. The
// executor hands it to the conductor verbatim (statusErr.msg is the raw body), so
// the matcher is tested against this shape rather than a compacted one.
const locationUnsupportedBody = `{
  "error": {
    "code": 400,
    "message": "User location is not supported for the API use.",
    "status": "FAILED_PRECONDITION"
  }
}`

// otherPreconditionBody is a FAILED_PRECONDITION about something else. It must
// stay an ordinary request fault: the matcher needs the location phrase too.
const otherPreconditionBody = `{"error":{"code":400,"message":"Project is not activated.","status":"FAILED_PRECONDITION"}}`

func newLocationUnsupportedError() error {
	return &statusBearingError{status: http.StatusBadRequest, msg: locationUnsupportedBody}
}

// registerLocationTestAuths registers one auth per id in the manager and in the
// global model registry, and unregisters them when the test ends.
func registerLocationTestAuths(t *testing.T, m *Manager, provider string, models []string, ids ...string) {
	t.Helper()
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model})
	}
	reg := registry.GetGlobalRegistry()
	for _, id := range ids {
		reg.RegisterClient(id, provider, infos)
		t.Cleanup(func() { reg.UnregisterClient(id) })
		if _, errRegister := m.Register(context.Background(), &Auth{ID: id, Provider: provider}); errRegister != nil {
			t.Fatalf("register auth %s: %v", id, errRegister)
		}
	}
}

func TestIsLocationUnsupportedMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "incident body", body: locationUnsupportedBody, want: true},
		{name: "compact", body: `{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`, want: true},
		{name: "message case and suffix", body: `{"error":{"message":"USER LOCATION IS NOT SUPPORTED for the API use without billing.","status":"FAILED_PRECONDITION"}}`, want: true},
		{name: "status case", body: `{"error":{"message":"User location is not supported for the API use.","status":"failed_precondition"}}`, want: true},
		{name: "response wrapper", body: `{"response":{"error":{"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}}`, want: true},
		{name: "body wrapper", body: `{"body":{"error":{"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}}`, want: true},

		{name: "other precondition message", body: otherPreconditionBody, want: false},
		{name: "location message with another status", body: `{"error":{"message":"User location is not supported for the API use.","status":"INVALID_ARGUMENT"}}`, want: false},
		{name: "location message without status", body: `{"error":{"code":400,"message":"User location is not supported for the API use."}}`, want: false},
		{name: "phrase only in details", body: `{"error":{"message":"nope","status":"FAILED_PRECONDITION","details":[{"reason":"User location is not supported"}]}}`, want: false},
		{name: "error is a string", body: `{"error":"User location is not supported for the API use."}`, want: false},
		{name: "plain text with the phrase", body: "User location is not supported for the API use.", want: false},
		{name: "wrapper text around json", body: `bad response status code 400, message: User location is not supported for the API use., body: ` + locationUnsupportedBody, want: false},
		{name: "empty", body: "", want: false},
		{name: "whitespace", body: "  \n", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLocationUnsupportedMessage(tc.body); got != tc.want {
				t.Fatalf("isLocationUnsupportedMessage(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestIsLocationUnsupportedError_RequiresBadRequestStatus(t *testing.T) {
	if !isLocationUnsupportedError(newLocationUnsupportedError()) {
		t.Fatal("a 400 carrying the location body must match")
	}
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if isLocationUnsupportedError(&statusBearingError{status: status, msg: locationUnsupportedBody}) {
			t.Fatalf("status %d must not match: the rejection is a 400 by contract", status)
		}
	}
	if isLocationUnsupportedError(errors.New(locationUnsupportedBody)) {
		t.Fatal("an error without an HTTP status must not match")
	}
	if isLocationUnsupportedError(nil) {
		t.Fatal("nil must not match")
	}
}

func TestLocationUnsupportedIsCredentialScoped(t *testing.T) {
	err := newLocationUnsupportedError()

	got := resultErrorFromError(err)
	if got == nil {
		t.Fatal("resultErrorFromError returned nil")
	}
	if got.Code != locationUnsupportedErrorCode {
		t.Fatalf("code = %q, want %q", got.Code, locationUnsupportedErrorCode)
	}
	if got.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("http status = %d, want %d", got.HTTPStatus, http.StatusBadRequest)
	}
	if isRequestInvalidError(err) {
		t.Fatal("a location rejection must not be a request fault: it has to rotate to another credential")
	}
	if shouldSkipCredentialCooldown(got) {
		t.Fatal("a location rejection must cool the credential, not skip the cooldown")
	}
	if !isLocationUnsupportedResultError(got) {
		t.Fatal("the classified result must be recognised as a location rejection")
	}
}

func TestLocationUnsupportedLeavesOtherBadRequestsAsRequestFaults(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "plain text", body: "bad request"},
		{name: "invalid argument", body: `{"error":{"code":400,"message":"Invalid JSON payload received.","status":"INVALID_ARGUMENT"}}`},
		{name: "other precondition", body: otherPreconditionBody},
		{name: "phrase without structured status", body: `{"error":{"code":400,"message":"User location is not supported for the API use."}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &statusBearingError{status: http.StatusBadRequest, msg: tc.body}
			if !isRequestInvalidError(err) {
				t.Fatal("an ordinary 400 must stay a request fault and stop credential rotation")
			}
			if got := resultErrorFromError(err); got == nil || got.Code != requestScopedErrorCode {
				t.Fatalf("code = %+v, want %q", got, requestScopedErrorCode)
			}
		})
	}
}

func TestManager_MarkResult_LocationUnsupportedCoolsModelWithoutEscalation(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	const provider = "antigravity"
	const model = "gemini-3.8-flash-high"
	const authID = "auth-location-cooldown"
	m := NewManager(nil, nil, nil)
	registerLocationTestAuths(t, m, provider, []string{model}, authID)

	result := Result{
		AuthID:   authID,
		Provider: provider,
		Model:    model,
		Success:  false,
		Error:    resultErrorFromError(newLocationUnsupportedError()),
	}
	reg := registry.GetGlobalRegistry()

	// Two consecutive hits: the window must be the same fixed length both times.
	for hit := 1; hit <= 2; hit++ {
		before := time.Now()
		m.MarkResult(context.Background(), result)

		updated, ok := m.GetByID(authID)
		if !ok || updated == nil {
			t.Fatalf("hit %d: expected auth to be present", hit)
		}
		state := updated.ModelStates[model]
		if state == nil {
			t.Fatalf("hit %d: expected a model state", hit)
		}
		if !state.Unavailable {
			t.Fatalf("hit %d: model must be unavailable while cooling", hit)
		}
		window := state.NextRetryAfter.Sub(before)
		if window < locationUnsupportedCooldown-5*time.Second || window > locationUnsupportedCooldown+5*time.Second {
			t.Fatalf("hit %d: cooldown window = %v, want ~%v with no escalation", hit, window, locationUnsupportedCooldown)
		}
		if state.StatusMessage != locationUnsupportedErrorCode || updated.StatusMessage != locationUnsupportedErrorCode {
			t.Fatalf("hit %d: status messages = %q / %q, want the marker %q", hit, state.StatusMessage, updated.StatusMessage, locationUnsupportedErrorCode)
		}
		if updated.LastError == nil || updated.LastError.Code != locationUnsupportedErrorCode || updated.LastError.HTTPStatus != http.StatusBadRequest {
			t.Fatalf("hit %d: last error = %+v, want code %q with status %d", hit, updated.LastError, locationUnsupportedErrorCode, http.StatusBadRequest)
		}
		if state.Quota.Exceeded || state.Quota.BackoffLevel != 0 || state.Quota.PaymentBackoffLevel != 0 || !state.Quota.NextRecoverAt.IsZero() {
			t.Fatalf("hit %d: quota state touched by a location rejection: %+v", hit, state.Quota)
		}
		if updated.Disabled {
			t.Fatalf("hit %d: a recoverable restriction must never disable the credential", hit)
		}
		// Only a cooldown: the model must stay registered, unlike a suspension.
		if count := reg.GetModelCount(model); count <= 0 {
			t.Fatalf("hit %d: model count = %d, want > 0 (the model must not be suspended)", hit, count)
		}
	}
}

func TestManager_MarkResult_LocationUnsupportedRespectsDisableCooling(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	const model = "gemini-3.8-flash-high"
	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-location-nocooling", Provider: "antigravity", Metadata: map[string]any{"disable_cooling": true}}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Model:    model,
		Success:  false,
		Error:    resultErrorFromError(newLocationUnsupportedError()),
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatal("expected a model state")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("disable_cooling must leave the credential schedulable, got NextRetryAfter=%v", state.NextRetryAfter)
	}
	if state.Unavailable {
		t.Fatal("disable_cooling must not leave the model unavailable")
	}
	if state.StatusMessage != locationUnsupportedErrorCode {
		t.Fatalf("status message = %q, want the marker so the UI can still flag it", state.StatusMessage)
	}
}

func TestManager_MarkResult_LocationUnsupportedAuthLevelCooldown(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-location-authlevel", Provider: "antigravity"}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	before := time.Now()
	// An empty model is how results that are not tied to a model are reported.
	m.MarkResult(context.Background(), Result{
		AuthID:   auth.ID,
		Provider: auth.Provider,
		Success:  false,
		Error:    resultErrorFromError(newLocationUnsupportedError()),
	})

	updated, ok := m.GetByID(auth.ID)
	if !ok || updated == nil {
		t.Fatal("expected auth to be present")
	}
	if !updated.Unavailable || updated.Status != StatusError {
		t.Fatalf("auth unavailable=%v status=%q, want unavailable with %q", updated.Unavailable, updated.Status, StatusError)
	}
	window := updated.NextRetryAfter.Sub(before)
	if window < locationUnsupportedCooldown-5*time.Second || window > locationUnsupportedCooldown+5*time.Second {
		t.Fatalf("cooldown window = %v, want ~%v", window, locationUnsupportedCooldown)
	}
	if updated.StatusMessage != locationUnsupportedErrorCode {
		t.Fatalf("status message = %q, want %q", updated.StatusMessage, locationUnsupportedErrorCode)
	}
	if updated.Quota.Exceeded || updated.Quota.BackoffLevel != 0 {
		t.Fatalf("quota state touched by a location rejection: %+v", updated.Quota)
	}
}

func TestManager_MarkResult_LocationUnsupportedNeverDisablesCredential(t *testing.T) {
	prev := disableCredentialInvalidAuths.Load()
	disableCredentialInvalidAuths.Store(true)
	t.Cleanup(func() {
		disableCredentialInvalidAuths.Store(prev)
		resetCredentialInvalidHits()
	})
	resetCredentialInvalidHits()

	const authID = "auth-location-keep"
	m := NewManager(nil, nil, nil)
	registerCredentialInvalidAuth(t, m, authID)

	// Far past the credential_invalid threshold, with auto-disable switched on.
	for range credentialInvalidHitsBeforeDisable + 3 {
		m.MarkResult(context.Background(), Result{
			AuthID:   authID,
			Provider: "antigravity",
			Model:    "gemini-3.8-flash-high",
			Success:  false,
			Error:    resultErrorFromError(newLocationUnsupportedError()),
		})
	}

	auth, ok := m.GetByID(authID)
	if !ok || auth == nil || auth.Disabled {
		t.Fatal("a region-restricted credential must never be auto-disabled: changing its proxy recovers it")
	}
	if _, counted := credentialInvalidHits.Load(authID); counted {
		t.Fatal("a location rejection must not count toward the credential_invalid strike threshold")
	}
}

func TestManager_MarkResult_SuccessLiftsLocationCooldownOnEveryModel(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	const provider = "antigravity"
	const authID = "auth-location-recovers"
	const servedModel = "gemini-3.8-flash-high"
	const idleModel = "gemini-3.1-pro-high"
	m := NewManager(nil, nil, nil)
	registerLocationTestAuths(t, m, provider, []string{servedModel, idleModel}, authID)

	locationFailure := func(model string) Result {
		return Result{
			AuthID:   authID,
			Provider: provider,
			Model:    model,
			Success:  false,
			Error:    resultErrorFromError(newLocationUnsupportedError()),
		}
	}
	m.MarkResult(context.Background(), locationFailure(idleModel))
	m.MarkResult(context.Background(), locationFailure(servedModel))

	// The proxy was fixed: only one model is requested again and it succeeds.
	m.MarkResult(context.Background(), Result{AuthID: authID, Provider: provider, Model: servedModel, Success: true})

	updated, ok := m.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected auth to be present")
	}
	// The model that was never requested again must not keep cooling, nor keep the
	// credential flagged: the restriction is credential-wide and has demonstrably lifted.
	idle := updated.ModelStates[idleModel]
	if idle == nil {
		t.Fatal("expected the idle model's state to be present")
	}
	if idle.Unavailable || !idle.NextRetryAfter.IsZero() || idle.LastError != nil || idle.StatusMessage != "" {
		t.Fatalf("idle model still carries the location state: %+v", idle)
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, idleModel, time.Now()); blocked {
		t.Fatal("the idle model must be schedulable again once the credential served a request")
	}
	if updated.LastError != nil || updated.StatusMessage != "" || updated.Status != StatusActive {
		t.Fatalf("auth still flagged after recovery: last_error=%+v status_message=%q status=%q", updated.LastError, updated.StatusMessage, updated.Status)
	}
}

func TestManager_MarkResult_SuccessKeepsOtherFailuresWhenLiftingLocation(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	const provider = "antigravity"
	const authID = "auth-location-mixed"
	const servedModel = "gemini-3.8-flash-high"
	const idleModel = "gemini-3.1-pro-high"
	const quotaModel = "claude-sonnet-4-6"
	m := NewManager(nil, nil, nil)
	registerLocationTestAuths(t, m, provider, []string{servedModel, idleModel, quotaModel}, authID)

	// An unrelated failure on one model, then the location rejection on two others.
	m.MarkResult(context.Background(), Result{
		AuthID:   authID,
		Provider: provider,
		Model:    quotaModel,
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota exhausted"},
	})
	for _, model := range []string{idleModel, servedModel} {
		m.MarkResult(context.Background(), Result{
			AuthID:   authID,
			Provider: provider,
			Model:    model,
			Success:  false,
			Error:    resultErrorFromError(newLocationUnsupportedError()),
		})
	}

	m.MarkResult(context.Background(), Result{AuthID: authID, Provider: provider, Model: servedModel, Success: true})

	updated, ok := m.GetByID(authID)
	if !ok || updated == nil {
		t.Fatal("expected auth to be present")
	}
	if idle := updated.ModelStates[idleModel]; idle == nil || idle.LastError != nil || idle.Unavailable {
		t.Fatalf("idle model must be cleared of the location state, got %+v", idle)
	}
	// The quota failure is a different fact about a different model: untouched.
	quota := updated.ModelStates[quotaModel]
	if quota == nil || quota.LastError == nil || !quota.Unavailable || quota.NextRetryAfter.IsZero() {
		t.Fatalf("the unrelated quota failure must survive, got %+v", quota)
	}
	// The credential is still in error because of that model, but what the list
	// reports as the last error must no longer be the stale location marker.
	if updated.Status != StatusError {
		t.Fatalf("status = %q, want %q while another model is still failing", updated.Status, StatusError)
	}
	if isLocationUnsupportedResultError(updated.LastError) || updated.StatusMessage == locationUnsupportedErrorCode {
		t.Fatalf("stale location marker left on the auth: last_error=%+v status_message=%q", updated.LastError, updated.StatusMessage)
	}
}

func TestManager_Execute_LocationUnsupportedRotatesToNextCredential(t *testing.T) {
	const provider = "antigravity"
	const model = "gemini-3.8-flash-high"
	const blockedID = "aa-location-blocked"
	const goodID = "bb-location-good"

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id:            provider,
		executeErrors: map[string]error{blockedID: newLocationUnsupportedError()},
	}
	m.RegisterExecutor(executor)
	registerLocationTestAuths(t, m, provider, []string{model}, blockedID, goodID)

	for i := 0; i < 2; i++ {
		resp, errExecute := m.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
		if errExecute != nil {
			t.Fatalf("request %d: expected the next credential to serve it, got error: %v", i, errExecute)
		}
		if got := string(resp.Payload); got != goodID {
			t.Fatalf("request %d: served by %q, want %q", i, got, goodID)
		}
	}

	// The first request walks blocked -> good. The second must not touch the
	// credential that is still cooling.
	want := []string{blockedID, goodID, goodID}
	got := executor.ExecuteCalls()
	if len(got) != len(want) {
		t.Fatalf("credential calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("credential calls = %v, want %v", got, want)
		}
	}

	updated, ok := m.GetByID(blockedID)
	if !ok || updated == nil {
		t.Fatal("expected the blocked credential to stay registered")
	}
	if blocked, _, _ := isAuthBlockedForModel(updated, model, time.Now()); !blocked {
		t.Fatal("expected the blocked credential to be cooling for the model")
	}
	if updated.Disabled {
		t.Fatal("the blocked credential must stay enabled")
	}
}

func TestManager_ExecuteStream_LocationUnsupportedRotatesToNextCredential(t *testing.T) {
	const provider = "antigravity"
	const model = "gemini-3.8-flash-high"
	const blockedID = "aa-location-stream-blocked"
	const goodID = "bb-location-stream-good"

	cases := []struct {
		name      string
		configure func(e *authFallbackExecutor)
	}{
		{
			// Antigravity reads the status before opening the stream, so the HTTP error
			// comes back directly rather than through the chunk channel.
			name: "direct error",
			configure: func(e *authFallbackExecutor) {
				e.streamDirectErrors = map[string]error{blockedID: newLocationUnsupportedError()}
			},
		},
		{
			name: "first chunk error",
			configure: func(e *authFallbackExecutor) {
				e.streamFirstErrors = map[string]error{blockedID: newLocationUnsupportedError()}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			executor := &authFallbackExecutor{id: provider}
			tc.configure(executor)
			m.RegisterExecutor(executor)
			registerLocationTestAuths(t, m, provider, []string{model}, blockedID, goodID)

			for i := 0; i < 2; i++ {
				streamResult, errStream := m.ExecuteStream(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
				if errStream != nil {
					t.Fatalf("stream %d: expected the next credential to serve it, got error: %v", i, errStream)
				}
				var payload []byte
				for chunk := range streamResult.Chunks {
					if chunk.Err != nil {
						t.Fatalf("stream %d: chunk error = %v, want success", i, chunk.Err)
					}
					payload = append(payload, chunk.Payload...)
				}
				if string(payload) != goodID {
					t.Fatalf("stream %d: payload = %q, want %q", i, string(payload), goodID)
				}
			}

			want := []string{blockedID, goodID, goodID}
			got := executor.StreamCalls()
			if len(got) != len(want) {
				t.Fatalf("stream calls = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("stream calls = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestManager_ExecuteCount_LocationUnsupportedRotatesToNextCredential(t *testing.T) {
	const provider = "antigravity"
	const model = "gemini-3.8-flash-high"
	const blockedID = "aa-location-count-blocked"
	const goodID = "bb-location-count-good"

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id:               provider,
		countTokenErrors: map[string]error{blockedID: newLocationUnsupportedError()},
	}
	m.RegisterExecutor(executor)
	registerLocationTestAuths(t, m, provider, []string{model}, blockedID, goodID)

	resp, errCount := m.ExecuteCount(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errCount != nil {
		t.Fatalf("expected the next credential to count the tokens, got error: %v", errCount)
	}
	if got := string(resp.Payload); got != goodID {
		t.Fatalf("counted by %q, want %q", got, goodID)
	}
}

func TestManager_Execute_LocationUnsupportedExhaustedPoolReturnsUpstreamBody(t *testing.T) {
	const provider = "antigravity"
	const model = "gemini-3.8-flash-high"
	const firstID = "aa-location-all-blocked"
	const secondID = "bb-location-all-blocked"

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: provider,
		executeErrors: map[string]error{
			firstID:  newLocationUnsupportedError(),
			secondID: newLocationUnsupportedError(),
		},
	}
	m.RegisterExecutor(executor)
	registerLocationTestAuths(t, m, provider, []string{model}, firstID, secondID)

	_, errExecute := m.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute == nil {
		t.Fatal("expected an error when every credential is region-restricted")
	}
	if calls := executor.ExecuteCalls(); len(calls) != 2 {
		t.Fatalf("credential calls = %v, want both credentials tried once", calls)
	}
	// The client must still see the upstream's own explanation.
	if got := errExecute.Error(); got != locationUnsupportedBody {
		t.Fatalf("error = %q, want the upstream body verbatim", got)
	}
}

func TestManager_Execute_OtherFailedPreconditionStillStopsRotation(t *testing.T) {
	const provider = "antigravity"
	const model = "gemini-3.8-flash-high"
	const firstID = "aa-precondition-first"
	const secondID = "bb-precondition-second"

	m := NewManager(nil, nil, nil)
	executor := &authFallbackExecutor{
		id: provider,
		executeErrors: map[string]error{
			firstID: &statusBearingError{status: http.StatusBadRequest, msg: otherPreconditionBody},
		},
	}
	m.RegisterExecutor(executor)
	registerLocationTestAuths(t, m, provider, []string{model}, firstID, secondID)

	if _, errExecute := m.Execute(context.Background(), []string{provider}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute == nil {
		t.Fatal("expected the 400 to be returned to the client")
	}
	// A FAILED_PRECONDITION that is not the location phrase is the request's fault:
	// trying another credential cannot help, so the walk stops at the first one.
	if calls := executor.ExecuteCalls(); len(calls) != 1 || calls[0] != firstID {
		t.Fatalf("credential calls = %v, want only [%s]", calls, firstID)
	}
}
