package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// The downloaded file is the only artifact an operator keeps after a sweep, so it
// has to stand on its own: the verdict counts, every failure, and the breakdowns
// that say what to do about them.
func TestDownloadCredentialSelfTestReportShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/credential-selftest/download", nil)
	h.DownloadCredentialSelfTestReport(ginCtx)

	// No run has finished yet, so there is nothing to hand back.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

// The exported document must carry the failure tier. It is the field that says
// whether a rejection needed the deep probe to find, which is what makes the deep
// probe's cost justifiable after the fact.
func TestCredentialSelfTestFailureEntryCarriesTier(t *testing.T) {
	entries := buildCredentialSelfTestFailures([]coreauth.SelfTestFailure{{
		AuthID:     "a",
		Provider:   "antigravity",
		StatusCode: http.StatusForbidden,
		Kind:       coreauth.SelfTestFailureDeterministic,
		Tier:       cliproxyauth.SelfTestTierGeneration,
	}})
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}
	if entries[0].Tier != int(cliproxyauth.SelfTestTierGeneration) {
		t.Fatalf("tier = %d, want %d", entries[0].Tier, int(cliproxyauth.SelfTestTierGeneration))
	}
}

// The 403 breakdown is the triage summary, and its whole value is keeping the
// recoverable kind apart from a ban. A 403 whose body said nothing recognizable
// must land in its own bucket rather than being guessed into either.
func TestCredentialSelfTestForbiddenBreakdown(t *testing.T) {
	failures := []coreauth.SelfTestFailure{
		{StatusCode: http.StatusForbidden, ForbiddenType: "validation"},
		{StatusCode: http.StatusForbidden, ForbiddenType: "validation"},
		{StatusCode: http.StatusForbidden, ForbiddenType: "violation"},
		{StatusCode: http.StatusForbidden},
		// Not a 403, so it must not appear in the breakdown at all.
		{StatusCode: http.StatusTooManyRequests, ForbiddenType: "validation"},
	}
	counts := credentialSelfTestForbiddenBreakdown(failures)
	if counts["validation"] != 2 {
		t.Fatalf("validation = %d, want 2", counts["validation"])
	}
	if counts["violation"] != 1 {
		t.Fatalf("violation = %d, want 1", counts["violation"])
	}
	if counts["unspecified"] != 1 {
		t.Fatalf("unspecified = %d, want 1", counts["unspecified"])
	}
	if len(counts) != 3 {
		t.Fatalf("expected exactly 3 buckets, got %v", counts)
	}

	if got := credentialSelfTestForbiddenBreakdown([]coreauth.SelfTestFailure{
		{StatusCode: http.StatusInternalServerError},
	}); got != nil {
		t.Fatalf("a run with no 403s must omit the breakdown, got %v", got)
	}
}

// The kind breakdown is what an operator reads first, so an unclassified failure
// must be visible as its own bucket rather than silently dropped from the total.
func TestCredentialSelfTestKindBreakdown(t *testing.T) {
	counts := credentialSelfTestKindBreakdown([]coreauth.SelfTestFailure{
		{Kind: coreauth.SelfTestFailureDeterministic},
		{Kind: coreauth.SelfTestFailureDeterministic},
		{Kind: coreauth.SelfTestFailureTransient},
		{Kind: coreauth.SelfTestFailureValidation},
		{Kind: ""},
	})
	if counts["deterministic"] != 2 || counts["transient"] != 1 || counts["validation"] != 1 {
		t.Fatalf("unexpected counts: %v", counts)
	}
	if counts["unknown"] != 1 {
		t.Fatalf("an unclassified failure must be counted as unknown, got %v", counts)
	}
	if counts["deterministic"]+counts["transient"]+counts["validation"]+counts["unknown"] != 5 {
		t.Fatalf("the buckets must account for every failure: %v", counts)
	}
}

// The export struct must round-trip through JSON with the field names the
// management API already promises, so a downloaded file and a live poll agree.
func TestCredentialSelfTestExportJSONKeys(t *testing.T) {
	export := credentialSelfTestExport{
		GeneratedAt: "2026-09-27T16:00:00Z",
		Report: credentialSelfTestResponse{
			StartedAt:  "2026-09-27T15:00:00Z",
			FinishedAt: "2026-09-27T15:02:00Z",
			Probed:     1802,
			Healthy:    1382,
			Failures: []credentialSelfTestFailureEntry{{
				AuthID: "a", StatusCode: 403, Kind: "deterministic", Tier: 2,
			}},
		},
		ByKind:      map[string]int{"deterministic": 1},
		ByForbidden: map[string]int{"validation": 1},
	}
	raw, errMarshal := json.Marshal(export)
	if errMarshal != nil {
		t.Fatalf("marshal: %v", errMarshal)
	}
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(raw, &decoded); errUnmarshal != nil {
		t.Fatalf("unmarshal: %v", errUnmarshal)
	}
	for _, key := range []string{"generated_at", "report", "failures_by_kind", "failures_by_forbidden_type"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("export is missing key %q: %s", key, raw)
		}
	}
	report, ok := decoded["report"].(map[string]any)
	if !ok {
		t.Fatalf("report is not an object: %s", raw)
	}
	for _, key := range []string{"probed", "healthy", "quota_exhausted", "not_probed", "failures"} {
		if _, ok := report[key]; !ok {
			t.Fatalf("report is missing key %q", key)
		}
	}
	failures, ok := report["failures"].([]any)
	if !ok || len(failures) != 1 {
		t.Fatalf("expected one failure entry, got %v", report["failures"])
	}
	entry, ok := failures[0].(map[string]any)
	if !ok {
		t.Fatalf("failure entry is not an object")
	}
	if _, ok := entry["tier"]; !ok {
		t.Fatalf("failure entry is missing the tier field: %v", entry)
	}
}
