package management

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func registerAntigravityModels(t *testing.T, authID string) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "antigravity", []*registry.ModelInfo{
		{ID: "gemini-3.7-flash-high"},
		{ID: "claude-sonnet-4-6"},
	})
	t.Cleanup(func() { reg.UnregisterClient(authID) })
}

func TestComputeAntigravityQuotaSummary(t *testing.T) {
	now := time.Now()

	t.Run("all available", func(t *testing.T) {
		authID := "auth-all-available"
		registerAntigravityModels(t, authID)
		a := &coreauth.Auth{ID: authID, Provider: "antigravity"}

		counts := computeAntigravityQuotaSummary([]*coreauth.Auth{a}, true, now)

		if counts.totalCredentials != 1 || counts.geminiAvailable != 1 || counts.claudeAvailable != 1 {
			t.Fatalf("unexpected counts: %+v", counts)
		}
		if counts.claudeAvailableViaCreditsOnly != 0 {
			t.Fatalf("expected no credits-only rescues, got %+v", counts)
		}
	})

	t.Run("all cooled down", func(t *testing.T) {
		authID := "auth-all-cooled-down"
		registerAntigravityModels(t, authID)
		future := now.Add(time.Hour)
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-3.7-flash-high": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
				"claude-sonnet-4-6":     {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
			},
		}

		counts := computeAntigravityQuotaSummary([]*coreauth.Auth{a}, true, now)

		if counts.totalCredentials != 1 || counts.geminiAvailable != 0 || counts.claudeAvailable != 0 {
			t.Fatalf("unexpected counts: %+v", counts)
		}
	})

	t.Run("mixed availability", func(t *testing.T) {
		geminiUpID := "auth-gemini-up"
		claudeUpID := "auth-claude-up"
		registerAntigravityModels(t, geminiUpID)
		registerAntigravityModels(t, claudeUpID)
		future := now.Add(time.Hour)

		geminiUp := &coreauth.Auth{
			ID:       geminiUpID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"claude-sonnet-4-6": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
			},
		}
		claudeUp := &coreauth.Auth{
			ID:       claudeUpID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"gemini-3.7-flash-high": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
			},
		}

		counts := computeAntigravityQuotaSummary([]*coreauth.Auth{geminiUp, claudeUp}, true, now)

		if counts.totalCredentials != 2 || counts.geminiAvailable != 1 || counts.claudeAvailable != 1 {
			t.Fatalf("unexpected counts: %+v", counts)
		}
	})

	t.Run("credits fallback rescues claude", func(t *testing.T) {
		authID := "auth-credits-rescue"
		registerAntigravityModels(t, authID)
		future := now.Add(time.Hour)
		coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{Known: true, Available: true})
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"claude-sonnet-4-6": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
			},
		}

		counts := computeAntigravityQuotaSummary([]*coreauth.Auth{a}, true, now)

		if counts.claudeAvailable != 1 || counts.claudeAvailableViaCreditsOnly != 1 {
			t.Fatalf("expected credits fallback rescue, got %+v", counts)
		}
	})

	t.Run("credits fallback disabled in config", func(t *testing.T) {
		authID := "auth-credits-disabled"
		registerAntigravityModels(t, authID)
		future := now.Add(time.Hour)
		coreauth.SetAntigravityCreditsHint(authID, coreauth.AntigravityCreditsHint{Known: true, Available: true})
		a := &coreauth.Auth{
			ID:       authID,
			Provider: "antigravity",
			ModelStates: map[string]*coreauth.ModelState{
				"claude-sonnet-4-6": {Quota: coreauth.QuotaState{Exceeded: true, NextRecoverAt: future}},
			},
		}

		counts := computeAntigravityQuotaSummary([]*coreauth.Auth{a}, false, now)

		if counts.claudeAvailable != 0 || counts.claudeAvailableViaCreditsOnly != 0 {
			t.Fatalf("expected no rescue when credits fallback disabled, got %+v", counts)
		}
	})

	t.Run("disabled auths excluded", func(t *testing.T) {
		authID := "auth-disabled"
		registerAntigravityModels(t, authID)
		a := &coreauth.Auth{ID: authID, Provider: "antigravity", Disabled: true}

		counts := computeAntigravityQuotaSummary([]*coreauth.Auth{a}, true, now)

		if counts.totalCredentials != 0 {
			t.Fatalf("expected disabled auth to be excluded, got %+v", counts)
		}
	})

	t.Run("non-antigravity provider excluded", func(t *testing.T) {
		authID := "auth-other-provider"
		registerAntigravityModels(t, authID)
		a := &coreauth.Auth{ID: authID, Provider: "codex"}

		counts := computeAntigravityQuotaSummary([]*coreauth.Auth{a}, true, now)

		if counts.totalCredentials != 0 {
			t.Fatalf("expected non-antigravity auth to be excluded, got %+v", counts)
		}
	})
}
