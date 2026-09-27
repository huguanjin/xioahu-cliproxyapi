package management

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// credentialSelfTestResponse is the report shape returned to the management UI.
// It mirrors the auth package's SelfTestReport but uses snake_case keys and
// omits the empty failure list so the common all-clear case stays small.
type credentialSelfTestResponse struct {
	StartedAt     string `json:"started_at"`
	FinishedAt    string `json:"finished_at"`
	Manual        bool   `json:"manual"`
	Concurrency   int    `json:"concurrency"`
	Timeout       string `json:"timeout"`
	Probed        int    `json:"probed"`
	Skipped       int    `json:"skipped"`
	Healthy       int    `json:"healthy"`
	Cooling       int    `json:"cooling"`
	Deterministic int    `json:"deterministic"`
	Escalated     int    `json:"escalated"`
	Transient     int    `json:"transient"`
	Validation    int    `json:"validation"`
	// QuotaExhausted is the share of Cooling that only the deep probe could
	// find: credentials whose generation quota is spent, which a cheap probe
	// passes.
	QuotaExhausted int `json:"quota_exhausted"`
	// NotProbed counts selected credentials whose provider has no probe to run.
	// Probed excludes them, so this is what accounts for the difference between
	// the pool size and Probed + Skipped.
	NotProbed int                              `json:"not_probed"`
	Failures  []credentialSelfTestFailureEntry `json:"failures,omitempty"`
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
	// ForbiddenType is the 403 subtype ("validation" or "violation") when the
	// provider could read one out of the body, empty otherwise.
	ForbiddenType string `json:"forbidden_type,omitempty"`
	// ValidationURL is the verification link that came with a validation 403.
	ValidationURL string `json:"validation_url,omitempty"`
}

// credentialSelfTestProgress is the live view of a run still in flight. Total and
// completed drive the progress bar; the verdict counts fill in behind it so the
// operator sees dead credentials pile up before the run ends.
type credentialSelfTestProgress struct {
	StartedAt      string                           `json:"started_at"`
	Manual         bool                             `json:"manual"`
	Concurrency    int                              `json:"concurrency"`
	Total          int                              `json:"total"`
	Completed      int                              `json:"completed"`
	Healthy        int                              `json:"healthy"`
	Cooling        int                              `json:"cooling"`
	Deterministic  int                              `json:"deterministic"`
	Escalated      int                              `json:"escalated"`
	Transient      int                              `json:"transient"`
	Validation     int                              `json:"validation"`
	QuotaExhausted int                              `json:"quota_exhausted"`
	NotProbed      int                              `json:"not_probed"`
	Failures       []credentialSelfTestFailureEntry `json:"failures,omitempty"`
}

// GetCredentialSelfTestStatus reports the schedule state, the running options,
// the run in flight if there is one, and the most recent completed run. It never
// probes: the UI polls this once a second while a run is in flight, so it must
// stay cheap.
func (h *Handler) GetCredentialSelfTestStatus(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	progress := h.authManager.CredentialSelfTestProgress()
	payload := gin.H{
		"schedule_enabled": h.authManager.CredentialSelfTestScheduleEnabled(),
		"running":          progress != nil,
	}
	if progress != nil {
		payload["progress"] = buildCredentialSelfTestProgress(progress)
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
	h.ensureSelfTestLoop()
	if !h.authManager.SetCredentialSelfTestScheduleEnabled(*body.Enabled) {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "credential self-test loop is not running"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"schedule_enabled": *body.Enabled})
}

// PostCredentialSelfTest starts a sweep in the background and returns at once.
//
// The run is asynchronous because it is not bounded by an HTTP request: a pool of
// a thousand-plus credentials takes far longer than any client will wait, and a
// browser that gives up must not take the run down with it. The caller polls
// GET /credential-selftest for progress and the finished report.
//
// The per-credential period is bypassed so a second press still probes. A run
// that would overlap one already in flight is refused rather than queued.
func (h *Handler) PostCredentialSelfTest(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	// No loop means nothing to run on, so start one. The schedule flag is
	// deliberately not consulted: the whole point of the manual trigger is to work
	// while the schedule is off, and a fresh loop starts with the schedule off.
	h.ensureSelfTestLoop()
	if !h.authManager.RunCredentialSelfTestNow() {
		c.JSON(http.StatusConflict, gin.H{"error": "a credential self-test run is already in progress"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "started"})
}

// ensureSelfTestLoop starts the self-test loop when none is running, reusing the
// options the manager already carries so a loop restarted from here keeps the
// YAML-derived concurrency, period and thresholds. An existing loop is left
// alone: restarting would drop its probe history and last report.
//
// No context is passed on: the loop outlives every request, so handing it one
// would tie its lifetime to a handler that returns immediately.
func (h *Handler) ensureSelfTestLoop() {
	if h == nil || h.authManager == nil || h.authManager.CredentialSelfTestRunning() {
		return
	}
	h.authManager.StartCredentialSelfTest(h.authManager.CredentialSelfTestOptions())
}

func buildCredentialSelfTestResponse(report *coreauth.SelfTestReport) credentialSelfTestResponse {
	response := credentialSelfTestResponse{
		StartedAt:      report.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt:     report.FinishedAt.UTC().Format(time.RFC3339),
		Manual:         report.Manual,
		Concurrency:    report.Concurrency,
		Timeout:        report.Timeout.String(),
		Probed:         report.Probed,
		Skipped:        report.Skipped,
		Healthy:        report.Verdict.Healthy,
		Cooling:        report.Verdict.Cooling,
		Deterministic:  report.Verdict.Deterministic,
		Escalated:      report.Verdict.Escalated,
		Transient:      report.Verdict.Transient,
		Validation:     report.Verdict.Validation,
		QuotaExhausted: report.Verdict.QuotaExhausted,
		NotProbed:      report.Verdict.NotProbed,
		Failures:       buildCredentialSelfTestFailures(report.Failures),
	}
	return response
}

func buildCredentialSelfTestProgress(progress *coreauth.SelfTestProgress) credentialSelfTestProgress {
	return credentialSelfTestProgress{
		StartedAt:      progress.StartedAt.UTC().Format(time.RFC3339),
		Manual:         progress.Manual,
		Concurrency:    progress.Concurrency,
		Total:          progress.Total,
		Completed:      progress.Completed,
		Healthy:        progress.Verdict.Healthy,
		Cooling:        progress.Verdict.Cooling,
		Deterministic:  progress.Verdict.Deterministic,
		Escalated:      progress.Verdict.Escalated,
		Transient:      progress.Verdict.Transient,
		Validation:     progress.Verdict.Validation,
		QuotaExhausted: progress.Verdict.QuotaExhausted,
		NotProbed:      progress.Verdict.NotProbed,
		Failures:       buildCredentialSelfTestFailures(progress.Failures),
	}
}

func buildCredentialSelfTestFailures(failures []coreauth.SelfTestFailure) []credentialSelfTestFailureEntry {
	if len(failures) == 0 {
		return nil
	}
	entries := make([]credentialSelfTestFailureEntry, 0, len(failures))
	for _, failure := range failures {
		entry := credentialSelfTestFailureEntry{
			AuthID:        failure.AuthID,
			Provider:      failure.Provider,
			Label:         failure.Label,
			StatusCode:    failure.StatusCode,
			Message:       failure.Message,
			Kind:          string(failure.Kind),
			Strikes:       failure.Strikes,
			ForbiddenType: failure.ForbiddenType,
			ValidationURL: failure.ValidationURL,
		}
		if !failure.CooldownUntil.IsZero() {
			entry.CooldownUntil = failure.CooldownUntil.UTC().Format(time.RFC3339)
		}
		entries = append(entries, entry)
	}
	return entries
}
