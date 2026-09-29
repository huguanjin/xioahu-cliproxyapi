package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// These pin the per-model detail behind a credential that reads as "down".
//
// The distinction under test is the whole reason the field exists: a spent
// window on one model ("cooldown", wait it out) is a different operational fact
// from a credential that cannot serve a model for some other reason
// ("blocked", may need a human). Collapsing them would rebuild, per credential,
// exactly the ambiguity the summary tiles already have.

// modelsFor registers an arbitrary model set for one credential, reusing the
// global registry the scheduler itself reads from.
func modelsFor(t *testing.T, authID string, models ...string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, id := range models {
		infos = append(infos, &registry.ModelInfo{ID: id})
	}
	reg.RegisterClient(authID, "antigravity", infos)
	t.Cleanup(func() { reg.UnregisterClient(authID) })
}

func TestBlockedModelsForAuth(t *testing.T) {
	now := time.Now()

	t.Run("reports nothing when every model is available", func(t *testing.T) {
		authID := "blocked-none"
		modelsFor(t, authID, "gemini-a", "claude-a")
		a := &coreauth.Auth{ID: authID, Provider: "antigravity"}

		if got := blockedModelsForAuth(a, now); len(got) != 0 {
			t.Fatalf("expected no blocked models, got %+v", got)
		}
	})

	t.Run("lists only the blocked models", func(t *testing.T) {
		// 健康的多数不列出来 —— 否则「哪里坏了」会变成要在一屏里自己找。
		authID := "blocked-some"
		modelsFor(t, authID, "gemini-a", "gemini-b", "claude-a")
		future := now.Add(30 * time.Minute)
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-b": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
			},
		}

		got := blockedModelsForAuth(a, now)
		if len(got) != 1 || got[0].ID != "gemini-b" {
			t.Fatalf("expected only gemini-b, got %+v", got)
		}
	})

	t.Run("calls an exceeded quota a cooldown with its recovery time", func(t *testing.T) {
		authID := "blocked-cooldown"
		modelsFor(t, authID, "claude-opus")
		future := now.Add(2 * time.Hour).Truncate(time.Second)
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"claude-opus": {
					Unavailable: true,
					Quota:       coreauth.QuotaState{Exceeded: true, NextRecoverAt: future},
				},
			},
		}

		got := blockedModelsForAuth(a, now)
		if len(got) != 1 {
			t.Fatalf("expected one entry, got %+v", got)
		}
		if got[0].Reason != "cooldown" {
			t.Fatalf("reason = %q, want cooldown", got[0].Reason)
		}
		want := future.UTC().Format(time.RFC3339)
		if got[0].RetryAt != want {
			t.Fatalf("retry_at = %q, want %q", got[0].RetryAt, want)
		}
	})

	t.Run("calls a non-quota block blocked, not cooldown", func(t *testing.T) {
		// 这两者混同会让操作者在一场只是等窗口恢复的故障里去找人为错误。
		authID := "blocked-other"
		modelsFor(t, authID, "gemini-a")
		future := now.Add(10 * time.Minute).Truncate(time.Second)
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-a": {
					Unavailable:    true,
					NextRetryAfter: future,
					StatusMessage:  "upstream rejected",
				},
			},
		}

		got := blockedModelsForAuth(a, now)
		if len(got) != 1 || got[0].Reason != "blocked" {
			t.Fatalf("expected a blocked entry, got %+v", got)
		}
		if got[0].StatusMessage != "upstream rejected" {
			t.Fatalf("status_message = %q, want the upstream text", got[0].StatusMessage)
		}
		if got[0].RetryAt == "" {
			t.Fatal("expected a retry_at for a block with a known expiry")
		}
	})

	t.Run("omits retry_at when the block has no known expiry", func(t *testing.T) {
		// 缺字段是诚实的；零值时间戳会被读成「已过期」。
		authID := "blocked-no-expiry"
		modelsFor(t, authID, "gemini-a")
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-a": {Unavailable: true},
			},
		}

		got := blockedModelsForAuth(a, now)
		if len(got) != 1 {
			t.Fatalf("expected one entry, got %+v", got)
		}
		if got[0].RetryAt != "" {
			t.Fatalf("retry_at = %q, want it omitted", got[0].RetryAt)
		}
	})

	t.Run("an elapsed quota window no longer counts as blocked", func(t *testing.T) {
		// availabilityBlock treats a recovery instant that has already passed as
		// AVAILABLE, so this model is not blocked at all. Reporting it would
		// invent a problem the scheduler does not have.
		authID := "blocked-expired-window"
		modelsFor(t, authID, "gemini-a")
		past := now.Add(-time.Hour)
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-a": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: past}},
			},
		}

		if got := blockedModelsForAuth(a, now); len(got) != 0 {
			t.Fatalf("an elapsed window should be available, got %+v", got)
		}
	})

	t.Run("sorts by model id so the list is stable across polls", func(t *testing.T) {
		authID := "blocked-order"
		modelsFor(t, authID, "zeta", "alpha", "mid")
		future := now.Add(time.Hour)
		states := map[string]*coreauth.ModelState{}
		for _, id := range []string{"zeta", "alpha", "mid"} {
			states[id] = &coreauth.ModelState{Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}}
		}
		a := &coreauth.Auth{ID: authID, Provider: "antigravity", ModelStates: states}

		got := blockedModelsForAuth(a, now)
		want := []string{"alpha", "mid", "zeta"}
		if len(got) != len(want) {
			t.Fatalf("expected %d entries, got %+v", len(want), got)
		}
		for i := range want {
			if got[i].ID != want[i] {
				t.Fatalf("order = %+v, want %v", got, want)
			}
		}
	})

	t.Run("ignores a disabled credential and other providers", func(t *testing.T) {
		authID := "blocked-disabled"
		modelsFor(t, authID, "gemini-a")
		future := now.Add(time.Hour)
		states := map[string]*coreauth.ModelState{
			"gemini-a": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
		}

		disabled := &coreauth.Auth{ID: authID, Provider: "antigravity", Disabled: true, ModelStates: states}
		if got := blockedModelsForAuth(disabled, now); len(got) != 0 {
			t.Fatalf("disabled credential should report nothing, got %+v", got)
		}

		other := &coreauth.Auth{ID: authID, Provider: "claude", ModelStates: states}
		if got := blockedModelsForAuth(other, now); len(got) != 0 {
			t.Fatalf("non-antigravity credential should report nothing, got %+v", got)
		}
	})

	t.Run("an unmatched state key means the model is available, not blocked", func(t *testing.T) {
		// isAuthBlockedForModel returns false (available) when no state matches the
		// model key — "auth-level availability can aggregate failures from other
		// models". So a case-mismatched key genuinely does NOT block, and this view
		// must agree: reporting it would invent a problem routing does not have.
		authID := "blocked-case"
		modelsFor(t, authID, "gemini-a")
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"GEMINI-A": {Unavailable: true},
			},
		}

		if got := blockedModelsForAuth(a, now); len(got) != 0 {
			t.Fatalf("an unmatched key should not block, got %+v", got)
		}
	})

	t.Run("a model whose own state is unavailable but has no expiry still reports", func(t *testing.T) {
		// 匹配到状态、且确实不可用，但没有记录恢复时刻 —— 仍然要报出来，
		// 只是没有 retry_at。漏报会让一个不可调度的模型看起来完全正常。
		authID := "blocked-no-expiry-matched"
		modelsFor(t, authID, "gemini-a")
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-a": {Unavailable: true},
			},
		}

		got := blockedModelsForAuth(a, now)
		if len(got) != 1 || got[0].ID != "gemini-a" {
			t.Fatalf("expected gemini-a to be reported, got %+v", got)
		}
		if got[0].Reason != "blocked" {
			t.Fatalf("reason = %q, want blocked", got[0].Reason)
		}
		if got[0].RetryAt != "" {
			t.Fatalf("retry_at = %q, want it omitted", got[0].RetryAt)
		}
	})

	t.Run("reports a model whose state is marked disabled", func(t *testing.T) {
		// state.Status == StatusDisabled 会被 isAuthBlockedForModel 直接判为
		// 不可用，所以它必须出现在列表里。
		authID := "blocked-state-disabled"
		modelsFor(t, authID, "gemini-a", "gemini-b")
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-a": {Status: coreauth.StatusDisabled},
			},
		}

		got := blockedModelsForAuth(a, now)
		if len(got) != 1 || got[0].ID != "gemini-a" {
			t.Fatalf("expected only gemini-a, got %+v", got)
		}
		if got[0].Reason != "blocked" {
			t.Fatalf("reason = %q, want blocked for a disabled model", got[0].Reason)
		}
	})

	t.Run("no registered models reports nothing", func(t *testing.T) {
		a := &coreauth.Auth{ID: "blocked-unregistered", Provider: "antigravity"}
		if got := blockedModelsForAuth(a, now); len(got) != 0 {
			t.Fatalf("expected nothing for an unregistered credential, got %+v", got)
		}
	})

	t.Run("nil auth is safe", func(t *testing.T) {
		if got := blockedModelsForAuth(nil, now); len(got) != 0 {
			t.Fatalf("expected nothing for nil auth, got %+v", got)
		}
	})
}
