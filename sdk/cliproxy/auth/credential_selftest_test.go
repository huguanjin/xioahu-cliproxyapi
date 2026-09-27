package auth

import (
	"context"
	"net/http"
	"strconv"
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

// A credential is skipped only when it is out of the loop's reach. Cooling and
// unavailable credentials stay due on their own cadence: the previous rule that
// skipped them meant the loop's own cooldown removed a credential from the only
// set that could ever clear it.
func TestCredentialSelfTestDue(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Minute)
	cases := []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "never probed", auth: &Auth{ID: "a", Provider: "antigravity"}, want: true},
		{name: "nil", auth: nil, want: false},
		{name: "no id", auth: &Auth{Provider: "antigravity"}, want: false},
		{name: "operator disabled", auth: &Auth{ID: "a", Disabled: true}, want: false},
		{
			name: "auto disabled stays due",
			auth: &Auth{ID: "a", Disabled: true, Metadata: map[string]any{
				"self_test": map[string]any{"auto_disabled": true},
			}},
			want: true,
		},
		// A credential the dispatcher is skipping still gets re-probed; that is
		// the whole point of keeping it in the queue.
		{name: "unavailable", auth: &Auth{ID: "a", Unavailable: true}, want: true},
		{name: "quota exceeded", auth: &Auth{ID: "a", Quota: QuotaState{Exceeded: true}}, want: true},
		{name: "retry pending", auth: &Auth{ID: "a", NextRetryAfter: future}, want: true},
		{
			name: "not due yet",
			auth: &Auth{ID: "a", Metadata: map[string]any{
				"self_test": map[string]any{"next_probe_at": future.UTC().Format(time.RFC3339)},
			}},
			want: false,
		},
		{
			name: "due again",
			auth: &Auth{ID: "a", Metadata: map[string]any{
				"self_test": map[string]any{"next_probe_at": past.UTC().Format(time.RFC3339)},
			}},
			want: true,
		},
		{
			// A hand-edited or corrupt timestamp must not pin a credential to a
			// future cadence forever.
			name: "corrupt timestamp reads as due",
			auth: &Auth{ID: "a", Metadata: map[string]any{
				"self_test": map[string]any{"next_probe_at": "not-a-time"},
			}},
			want: true,
		},
	}
	for _, tc := range cases {
		if got := selfTestDue(tc.auth, time.Now(), true); got != tc.want {
			t.Errorf("%s: expected due=%v, got %v", tc.name, tc.want, got)
		}
	}
}

// A manual run asks what is true now, so it must not hand back the schedule's
// own answer: a credential the last scheduled sweep pushed an hour out is still
// probed when an operator presses the button.
func TestCredentialSelfTestDueIgnoresCadenceOnManualRun(t *testing.T) {
	parked := &Auth{ID: "a", Metadata: map[string]any{
		"self_test": map[string]any{"next_probe_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	}}
	if selfTestDue(parked, time.Now(), true) {
		t.Fatalf("a parked credential must not be due on a scheduled sweep")
	}
	if !selfTestDue(parked, time.Now(), false) {
		t.Fatalf("a manual run must probe a parked credential anyway")
	}

	// The reachability rules still apply: a manual run does not resurrect a
	// credential an operator disabled by hand.
	operatorDisabled := &Auth{ID: "b", Disabled: true}
	if selfTestDue(operatorDisabled, time.Now(), false) {
		t.Fatalf("a manual run must still skip an operator-disabled credential")
	}
}

// The cadence lives on the credential, so the loop must not re-probe within the
// period even though nothing is tracked in memory any more.
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
	loop := newSelfTestLoopForTest(manager, SelfTestOptions{
		Interval:            time.Minute,
		PerCredentialPeriod: time.Hour,
		Timeout:             time.Second,
		Concurrency:         2,
	})

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
	loop := newSelfTestLoopForTest(manager, SelfTestOptions{
		Interval:            time.Minute,
		PerCredentialPeriod: time.Hour,
		Timeout:             time.Second,
		Concurrency:         2,
	})
	// Give the credential a cadence far in the future; a manual run must ignore it.
	loop.candidates(time.Now(), true)

	due, skipped := loop.candidates(time.Now(), false)
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

// The deep-probe sample must be stable, not random. A random draw re-picks the
// same credential twice in a row as often as not, which would leave part of the
// pool untouched for long stretches while the rest is deep probed repeatedly.
// Stability is what lets the sample cover the pool over successive sweeps.
func TestDeepProbeSampledIsStable(t *testing.T) {
	loop := &credentialSelfTestLoop{}
	sampled := make(map[string]bool)
	for _, id := range []string{"a", "b", "c", "alpha", "beta", "gamma", "credential-0001", "credential-0002"} {
		first := loop.deepProbeSampled(id, 50)
		for i := 0; i < 8; i++ {
			if again := loop.deepProbeSampled(id, 50); again != first {
				t.Fatalf("%s changed its sample verdict between calls: %v then %v", id, first, again)
			}
		}
		sampled[id] = first
	}
	// A 50% sample that picked everything or nothing would be a broken hash rather
	// than a sample; with eight ids the odds of either extreme are negligible.
	selected := 0
	for _, ok := range sampled {
		if ok {
			selected++
		}
	}
	if selected == 0 || selected == len(sampled) {
		t.Fatalf("50%% sample selected %d of %d ids, which is not a sample", selected, len(sampled))
	}
}

// The boundaries are what an operator configures against: 0 must mean "never
// deep probe" and 100 must mean "always", and neither may fall through the hash.
func TestDeepProbeSampledBoundaries(t *testing.T) {
	loop := &credentialSelfTestLoop{}
	if loop.deepProbeSampled("a", 0) {
		t.Fatalf("a 0%% sample must select nothing")
	}
	if !loop.deepProbeSampled("a", 100) {
		t.Fatalf("a 100%% sample must select everything")
	}
	if loop.deepProbeSampled("a", -1) {
		t.Fatalf("a negative sample must select nothing")
	}
	// An empty id must not be sampled: it cannot be hashed meaningfully, and the
	// stable-selection property does not hold for it.
	if loop.deepProbeSampled("  ", 100) {
		t.Fatalf("an empty credential id must not be sampled")
	}
}

// Roughly the configured share of a pool must be selected, so a small percentage
// is a real throttle rather than an accidental full sweep. The hash is stable, so
// this is a deterministic assertion about the ids, not a statistical one.
func TestDeepProbeSampledApproximatesConfiguredShare(t *testing.T) {
	loop := &credentialSelfTestLoop{}
	ids := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		ids = append(ids, "credential-"+strconv.Itoa(i))
	}
	for _, percent := range []int{2, 10, 50} {
		selected := 0
		for _, id := range ids {
			if loop.deepProbeSampled(id, percent) {
				selected++
			}
		}
		// FNV-1a mod 100 over 1000 ids should land within a few points of the
		// target. A wide band keeps the test about gross correctness, not about
		// pinning a hash that may be legitimately replaced.
		if selected < percent*5 || selected > percent*15 {
			t.Fatalf("%d%% selected %d of %d ids, want roughly %d", percent, selected, len(ids), percent*10)
		}
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

// The loop's lifetime must not be tied to whatever request happened to start it.
// A manual run is launched from an HTTP handler, and that request's context is
// cancelled the moment the handler returns its 202 — which is immediately. When
// the loop inherited it, the sweep died before launching its first probe and the
// report came back with probed=0 and identical start and finish timestamps while
// the pool was full of credentials that should have been examined.
//
// StartCredentialSelfTest now takes no context, so the type system carries most
// of this. The test remains as the end-to-end guard: a loop started the way the
// management handler starts it, then triggered at once, must actually probe.
func TestCredentialSelfTestLoopSurvivesItsStarterContext(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
		ID:       "a",
		Provider: "antigravity",
		Status:   StatusActive,
	}); errRegister != nil {
		t.Fatalf("Register returned error: %v", errRegister)
	}
	manager.RegisterExecutor(stubExecutor{id: "antigravity"})

	manager.StartCredentialSelfTest(SelfTestOptions{
		Interval:            time.Minute,
		PerCredentialPeriod: time.Minute,
		Timeout:             time.Second,
		Concurrency:         1,
	})
	if !manager.RunCredentialSelfTestNow() {
		t.Fatal("expected the manual run to be accepted")
	}

	// The sweep runs in the background; wait for it to report rather than sleeping
	// a fixed period, so the test fails on the real defect instead of on timing.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if report := manager.LastCredentialSelfTestReport(); report != nil {
			if report.Probed != 1 {
				t.Fatalf("a manual run must probe the pool; got probed=%d skipped=%d",
					report.Probed, report.Skipped)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the run never produced a report")
}

// A config reload must reach a loop that is already running. Before this, the
// options were fixed at construction: raising probe concurrency in config.yaml was
// accepted, logged, and then ignored until the process restarted, so an operator
// tuning a long sweep could not tell whether their change had taken effect.
func TestApplySelfTestOptionsUpdatesRunningLoop(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(stubExecutor{id: "antigravity"})

	manager.StartCredentialSelfTest(SelfTestOptions{
		Interval:            time.Minute,
		PerCredentialPeriod: time.Minute,
		Timeout:             time.Second,
		Concurrency:         4,
	})
	defer manager.StopCredentialSelfTest()

	if got := manager.CredentialSelfTestOptions().Concurrency; got != 4 {
		t.Fatalf("before reload concurrency = %d, want 4", got)
	}
	if !manager.ApplySelfTestOptions(SelfTestOptions{
		Interval:            time.Minute,
		PerCredentialPeriod: time.Minute,
		Timeout:             time.Second,
		Concurrency:         32,
	}) {
		t.Fatal("ApplySelfTestOptions reported no loop to update")
	}
	if got := manager.CredentialSelfTestOptions().Concurrency; got != 32 {
		t.Fatalf("after reload concurrency = %d, want 32", got)
	}
}

// The schedule toggle is runtime state, not configuration. An operator who turned
// the schedule off must not have it switched back on by an unrelated config edit.
func TestApplySelfTestOptionsLeavesScheduleAlone(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(stubExecutor{id: "antigravity"})

	manager.StartCredentialSelfTest(SelfTestOptions{
		Enabled:             false,
		Interval:            time.Minute,
		PerCredentialPeriod: time.Minute,
		Timeout:             time.Second,
		Concurrency:         4,
	})
	defer manager.StopCredentialSelfTest()
	if manager.CredentialSelfTestScheduleEnabled() {
		t.Fatal("the loop must start with the schedule off when the option says so")
	}

	// A reload that turns the schedule on in YAML must not flip the live toggle.
	manager.ApplySelfTestOptions(SelfTestOptions{
		Enabled:             true,
		Interval:            time.Minute,
		PerCredentialPeriod: time.Minute,
		Timeout:             time.Second,
		Concurrency:         4,
	})
	if manager.CredentialSelfTestScheduleEnabled() {
		t.Fatal("a config reload must not switch the schedule on behind the operator")
	}
}

// Options arriving while no loop exists must still be waiting when one is created,
// otherwise the reload would be undone by the next manual run falling back to
// defaults.
func TestPublishSelfTestOptionsSurvivesUntilLoopStarts(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(stubExecutor{id: "antigravity"})

	manager.PublishSelfTestOptions(SelfTestOptions{
		Interval:            time.Minute,
		PerCredentialPeriod: time.Minute,
		Timeout:             time.Second,
		Concurrency:         16,
	})
	if manager.CredentialSelfTestRunning() {
		t.Fatal("publishing options must not start a loop")
	}
	if got := manager.CredentialSelfTestOptions().Concurrency; got != 16 {
		t.Fatalf("published concurrency = %d, want 16", got)
	}

	// The management handler starts the loop from exactly these options.
	manager.StartCredentialSelfTest(manager.CredentialSelfTestOptions())
	defer manager.StopCredentialSelfTest()
	if got := manager.CredentialSelfTestOptions().Concurrency; got != 16 {
		t.Fatalf("a loop started from the published options got concurrency %d, want 16", got)
	}
}

// The loop's options are read by probe goroutines while a reload rewrites them, so
// the access must be race-free. Run with -race to make this meaningful.
func TestApplySelfTestOptionsIsRaceFree(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(stubExecutor{id: "antigravity"})
	for i := 0; i < 8; i++ {
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), &Auth{
			ID:       "auth-" + strconv.Itoa(i),
			Provider: "antigravity",
			Status:   StatusActive,
		}); errRegister != nil {
			t.Fatalf("Register returned error: %v", errRegister)
		}
	}

	manager.StartCredentialSelfTest(SelfTestOptions{
		Interval:            time.Minute,
		PerCredentialPeriod: time.Millisecond,
		Timeout:             time.Second,
		Concurrency:         4,
	})
	defer manager.StopCredentialSelfTest()
	manager.RunCredentialSelfTestNow()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			manager.ApplySelfTestOptions(SelfTestOptions{
				Interval:            time.Minute,
				PerCredentialPeriod: time.Millisecond,
				Timeout:             time.Second,
				Concurrency:         4 + i%8,
			})
		}
	}()
	for i := 0; i < 200; i++ {
		_ = manager.CredentialSelfTestOptions()
		_ = manager.CredentialSelfTestScheduleEnabled()
	}
	<-done
}

// newSelfTestLoopForTest builds a loop the way StartCredentialSelfTest does,
// including publishing its options. Tests that drive loop methods directly need
// this because the options now live in an atomic rather than a plain field.
func newSelfTestLoopForTest(manager *Manager, options SelfTestOptions) *credentialSelfTestLoop {
	loop := &credentialSelfTestLoop{manager: manager, ctx: context.Background()}
	loop.optionsValue.Store(options.Normalize())
	return loop
}
