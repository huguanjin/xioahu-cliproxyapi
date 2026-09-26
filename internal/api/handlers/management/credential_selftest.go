package management

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// credentialSelfTestResponse is the report shape returned to the management UI.
// It mirrors the auth package's SelfTestReport but uses snake_case keys and
// omits the empty failure list so the common all-clear case stays small.
type credentialSelfTestResponse struct {
	StartedAt     string                           `json:"started_at"`
	FinishedAt    string                           `json:"finished_at"`
	Manual        bool                             `json:"manual"`
	Concurrency   int                              `json:"concurrency"`
	Timeout       string                           `json:"timeout"`
	Probed        int                              `json:"probed"`
	Skipped       int                              `json:"skipped"`
	Healthy       int                              `json:"healthy"`
	Cooling       int                              `json:"cooling"`
	Deterministic int                              `json:"deterministic"`
	Escalated     int                              `json:"escalated"`
	Transient     int                              `json:"transient"`
	Failures      []credentialSelfTestFailureEntry `json:"failures,omitempty"`
}

type credentialSelfTestFailureEntry struct {
	AuthID        string `json:"auth_id"`
	Provider      string `json:"provider"`
	Label         string `json:"label,omitempty"`
	StatusCode    int    `json:"status_code"`
	Message       string `json:"message,omitempty"`
	Kind          string `json:"kind"`
	Strikes       int    `json:"strikes"`
	CooldownUntil string `json:"cooldown_until,omitempty"`
}

// GetCredentialSelfTestStatus reports the schedule state, the running options
// and the most recent completed run. It never probes: the UI polls this while a
// run is in flight and after, so it must stay cheap.
func (h *Handler) GetCredentialSelfTestStatus(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	payload := gin.H{
		"schedule_enabled": h.authManager.CredentialSelfTestScheduleEnabled(),
	}
	if report := h.authManager.LastCredentialSelfTestReport(); report != nil {
		payload["last_report"] = buildCredentialSelfTestResponse(report)
	}
	c.JSON(http.StatusOK, payload)
}

// PatchCredentialSelfTest toggles the periodic schedule without tearing down the
// loop, so a manual trigger keeps working while the schedule is off.
func (h *Handler) PatchCredentialSelfTest(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled is required"})
		return
	}
	// A loop must exist for the switch to mean anything; start one if the
	// service has not done so yet (embedders that never call Service.Run).
	h.ensureSelfTestLoop(c.Request.Context())
	if !h.authManager.SetCredentialSelfTestScheduleEnabled(*body.Enabled) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential self-test loop is not running"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedule_enabled": *body.Enabled})
}

// PostCredentialSelfTest runs one sweep immediately, ignoring the schedule and
// the per-credential period, and returns the finished report.
//
// The run is synchronous because the operator is watching for the answer; it
// bypasses the per-credential period so a second press still probes. A run that
// would overlap one already in flight is refused rather than queued.
func (h *Handler) PostCredentialSelfTest(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	// No loop means nothing to run on, so start one. The schedule flag is
	// deliberately not consulted: the whole point of the manual trigger is to work
	// while the schedule is off, and a fresh loop starts with the schedule off.
	h.ensureSelfTestLoop(c.Request.Context())
	report := h.authManager.RunCredentialSelfTestNow(c.Request.Context())
	if report == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "a credential self-test run is already in progress"})
		return
	}
	c.JSON(http.StatusOK, buildCredentialSelfTestResponse(report))
}

// ensureSelfTestLoop starts the self-test loop when none is running, reusing the
// options the manager already carries so a loop restarted from here keeps the
// YAML-derived concurrency, period and thresholds. An existing loop is left
// alone: restarting would drop its probe history and last report.
func (h *Handler) ensureSelfTestLoop(ctx context.Context) {
	if h == nil || h.authManager == nil || h.authManager.CredentialSelfTestRunning() {
		return
	}
	h.authManager.StartCredentialSelfTest(ctx, h.authManager.CredentialSelfTestOptions())
}

func buildCredentialSelfTestResponse(report *coreauth.SelfTestReport) credentialSelfTestResponse {
	response := credentialSelfTestResponse{
		StartedAt:     report.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt:    report.FinishedAt.UTC().Format(time.RFC3339),
		Manual:        report.Manual,
		Concurrency:   report.Concurrency,
		Timeout:       report.Timeout.String(),
		Probed:        report.Probed,
		Skipped:       report.Skipped,
		Healthy:       report.Verdict.Healthy,
		Cooling:       report.Verdict.Cooling,
		Deterministic: report.Verdict.Deterministic,
		Escalated:     report.Verdict.Escalated,
		Transient:     report.Verdict.Transient,
	}
	for _, failure := range report.Failures {
		entry := credentialSelfTestFailureEntry{
			AuthID:     failure.AuthID,
			Provider:   failure.Provider,
			Label:      failure.Label,
			StatusCode: failure.StatusCode,
			Message:    failure.Message,
			Kind:       string(failure.Kind),
			Strikes:    failure.Strikes,
		}
		if !failure.CooldownUntil.IsZero() {
			entry.CooldownUntil = failure.CooldownUntil.UTC().Format(time.RFC3339)
		}
		response.Failures = append(response.Failures, entry)
	}
	return response
}
