package management

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// antigravityQuotaSummaryCounts tallies antigravity credential availability
// for the Gemini and Claude model families, based on local cooldown/quota
// state and (for Claude) the antigravity-credits fallback eligibility.
type antigravityQuotaSummaryCounts struct {
	totalCredentials                int
	geminiAvailable                 int
	claudeAvailable                 int
	claudeAvailableViaCreditsOnly   int
	claudeCreditsFallbackConfigured bool
}

// computeAntigravityQuotaSummary aggregates schedulability across all
// antigravity auths. creditsFallbackEnabled mirrors the
// quota-exceeded.antigravity-credits config toggle.
func computeAntigravityQuotaSummary(auths []*coreauth.Auth, creditsFallbackEnabled bool, now time.Time) antigravityQuotaSummaryCounts {
	counts := antigravityQuotaSummaryCounts{claudeCreditsFallbackConfigured: creditsFallbackEnabled}

	for _, a := range auths {
		if a == nil || a.Disabled || !strings.EqualFold(strings.TrimSpace(a.Provider), "antigravity") {
			continue
		}

		models := registry.GetGlobalRegistry().GetModelsForClient(a.ID)
		if len(models) == 0 {
			continue
		}

		counts.totalCredentials++

		geminiAvailable := false
		claudeFreeAvailable := false
		for _, m := range models {
			if m == nil || m.ID == "" {
				continue
			}
			if strings.Contains(strings.ToLower(m.ID), "claude") {
				if !claudeFreeAvailable && coreauth.IsModelAvailable(a, m.ID, now) {
					claudeFreeAvailable = true
				}
				continue
			}
			if !geminiAvailable && coreauth.IsModelAvailable(a, m.ID, now) {
				geminiAvailable = true
			}
		}

		if geminiAvailable {
			counts.geminiAvailable++
		}

		claudeAvailable := claudeFreeAvailable
		if !claudeAvailable && creditsFallbackEnabled {
			if hint, ok := coreauth.GetAntigravityCreditsHint(a.ID); ok && hint.Known && hint.Available {
				claudeAvailable = true
				counts.claudeAvailableViaCreditsOnly++
			}
		}
		if claudeAvailable {
			counts.claudeAvailable++
		}
	}

	return counts
}

// GetAntigravityQuotaSummary reports, across all antigravity credentials, how
// many currently have schedulable Gemini quota and how many have schedulable
// Claude quota (including credits-fallback eligibility). Counts are derived
// entirely from local scheduling state; no live upstream calls are made.
func (h *Handler) GetAntigravityQuotaSummary(c *gin.Context) {
	var auths []*coreauth.Auth
	if h.authManager != nil {
		auths = h.authManager.List()
	}

	creditsEnabled := h.cfg != nil && h.cfg.QuotaExceeded.AntigravityCredits
	counts := computeAntigravityQuotaSummary(auths, creditsEnabled, time.Now())

	c.JSON(200, gin.H{
		"generated_at":                  time.Now().UTC().Format(time.RFC3339),
		"total_antigravity_credentials": counts.totalCredentials,
		"gemini": gin.H{
			"available": counts.geminiAvailable,
			"total":     counts.totalCredentials,
		},
		"claude": gin.H{
			"available":                           counts.claudeAvailable,
			"total":                               counts.totalCredentials,
			"available_via_credits_fallback_only": counts.claudeAvailableViaCreditsOnly,
			"credits_fallback_enabled":            counts.claudeCreditsFallbackConfigured,
		},
	})
}
