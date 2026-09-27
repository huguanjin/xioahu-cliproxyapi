package management

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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
	AuthID     string `json:"auth_id"`
	Provider   string `json:"provider"`
	Label      string `json:"label,omitempty"`
	StatusCode int    `json:"status_code"`
	Message    string `json:"message,omitempty"`
	Kind       string `json:"kind"`
	Strikes    int    `json:"strikes"`
	// Tier is which probe produced this failure: 1 is the cheap authorization
	// check, 2 the real generation call. It is the field that tells a rejection a
	// cheap probe would also have caught from one only the deep probe could see.
	Tier          int    `json:"tier"`
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

// DownloadCredentialSelfTestReport returns the last completed run as a file.
//
// It exists because a large pool's report is too big to read in a dialog: the
// verdict table and the failure list are what an operator needs to work through
// the dead credentials one by one, and 400 failures is not something a panel can
// show usefully.
//
// The download carries more than the status endpoint does. The status answer is
// polled once a second, so it is kept small; this one is fetched once and can
// afford the whole failure list, along with a summary and per-verdict breakdown
// that make the file self-explanatory when it is opened on its own.
func (h *Handler) DownloadCredentialSelfTestReport(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	report := h.authManager.LastCredentialSelfTestReport()
	if report == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no completed credential self-test run"})
		return
	}

	export := credentialSelfTestExport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Report:      buildCredentialSelfTestResponse(report),
		ByKind:      credentialSelfTestKindBreakdown(report.Failures),
		ByForbidden: credentialSelfTestForbiddenBreakdown(report.Failures),
	}
	// Marshal through the same struct the API uses, then re-indent, so the file
	// and the endpoint can never disagree about the field names.
	raw, errMarshal := json.Marshal(export)
	if errMarshal != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to encode report"})
		return
	}
	var buf bytes.Buffer
	if errIndent := json.Indent(&buf, raw, "", "  "); errIndent != nil {
		buf.Write(raw)
	}

	name := fmt.Sprintf("credential-selftest-%s.json", report.StartedAt.UTC().Format("20060102-150405"))
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
	c.Data(http.StatusOK, "application/json; charset=utf-8", buf.Bytes())
}

// credentialSelfTestExport is the downloaded document. It wraps the report rather
// than being it, so the summary fields can be added without changing the shape
// the status endpoint promises.
type credentialSelfTestExport struct {
	GeneratedAt string                     `json:"generated_at"`
	Report      credentialSelfTestResponse `json:"report"`
	// ByKind and ByForbidden answer the questions an operator asks of a failure
	// list before reading any of it: how many of each verdict, and for 403s, how
	// many are the recoverable validation kind rather than a real ban.
	ByKind      map[string]int `json:"failures_by_kind"`
	ByForbidden map[string]int `json:"failures_by_forbidden_type,omitempty"`
}

// credentialSelfTestKindBreakdown counts failures by verdict kind. A failure list
// is read to decide what to do about each credential, and that starts with how
// many of each kind there are.
func credentialSelfTestKindBreakdown(failures []coreauth.SelfTestFailure) map[string]int {
	counts := make(map[string]int, 3)
	for _, failure := range failures {
		kind := string(failure.Kind)
		if kind == "" {
			kind = "unknown"
		}
		counts[kind]++
	}
	return counts
}

// credentialSelfTestForbiddenBreakdown counts 403 failures by the subtype the
// executor read from the body. The distinction is the whole point of the 403
// triage: "validation" means the account owner can still clear it, and a ban does
// not. A 403 whose body carried no recognizable subtype is counted as
// "unspecified" rather than folded into either, because guessing has already
// proven to be the expensive mistake here.
func credentialSelfTestForbiddenBreakdown(failures []coreauth.SelfTestFailure) map[string]int {
	counts := make(map[string]int)
	for _, failure := range failures {
		if failure.StatusCode != http.StatusForbidden {
			continue
		}
		subtype := strings.TrimSpace(failure.ForbiddenType)
		if subtype == "" {
			subtype = "unspecified"
		}
		counts[subtype]++
	}
	if len(counts) == 0 {
		return nil
	}
	return counts
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
			Tier:          int(failure.Tier),
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
