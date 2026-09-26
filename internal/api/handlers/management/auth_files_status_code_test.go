package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestListAuthFilesExposesLastStatusCode(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	writeAuthFileForStatusCodeTest(t, authDir, "forbidden.json")
	writeAuthFileForStatusCodeTest(t, authDir, "healthy.json")

	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:        "auth-forbidden",
		FileName:  "forbidden.json",
		Provider:  "antigravity",
		Status:    coreauth.StatusError,
		LastError: &coreauth.Error{HTTPStatus: http.StatusForbidden, Code: "payment_required"},
		Attributes: map[string]string{
			"path": filepath.Join(authDir, "forbidden.json"),
		},
	})
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:       "auth-healthy",
		FileName: "healthy.json",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filepath.Join(authDir, "healthy.json"),
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	files := listAuthFilesForStatusCodeTest(t, h)
	if len(files) != 2 {
		t.Fatalf("files len = %d, want 2", len(files))
	}

	byID := make(map[string]map[string]any, len(files))
	for _, file := range files {
		id, _ := file["id"].(string)
		byID[id] = file
	}

	forbidden, ok := byID["auth-forbidden"]
	if !ok {
		t.Fatalf("missing auth-forbidden in %#v", files)
	}
	// JSON numbers decode as float64.
	if code, _ := forbidden["last_status_code"].(float64); code != http.StatusForbidden {
		t.Fatalf("last_status_code = %#v, want 403", forbidden["last_status_code"])
	}
	if code, _ := forbidden["last_error_code"].(string); code != "payment_required" {
		t.Fatalf("last_error_code = %#v, want payment_required", forbidden["last_error_code"])
	}

	healthy, ok := byID["auth-healthy"]
	if !ok {
		t.Fatalf("missing auth-healthy in %#v", files)
	}
	// Credentials that never failed must not carry a zero status code.
	for _, key := range []string{"last_status_code", "last_error_code"} {
		if _, present := healthy[key]; present {
			t.Fatalf("%s = %#v on a credential without LastError, want the field absent", key, healthy[key])
		}
	}
}

func TestListAuthFilesExposesStatusCodeWithoutErrorCode(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	authDir := t.TempDir()
	writeAuthFileForStatusCodeTest(t, authDir, "throttled.json")

	manager := coreauth.NewManager(nil, nil, nil)
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:        "auth-throttled",
		FileName:  "throttled.json",
		Provider:  "antigravity",
		Status:    coreauth.StatusError,
		LastError: &coreauth.Error{HTTPStatus: http.StatusTooManyRequests},
		Attributes: map[string]string{
			"path": filepath.Join(authDir, "throttled.json"),
		},
	})

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	files := listAuthFilesForStatusCodeTest(t, h)
	if len(files) != 1 {
		t.Fatalf("files len = %d, want 1", len(files))
	}
	if code, _ := files[0]["last_status_code"].(float64); code != http.StatusTooManyRequests {
		t.Fatalf("last_status_code = %#v, want 429", files[0]["last_status_code"])
	}
	if _, present := files[0]["last_error_code"]; present {
		t.Fatalf("last_error_code = %#v, want the field absent when the error carries no code", files[0]["last_error_code"])
	}
}

func writeAuthFileForStatusCodeTest(t *testing.T, authDir string, name string) {
	t.Helper()
	path := filepath.Join(authDir, name)
	if errWrite := os.WriteFile(path, []byte(`{"type":"antigravity"}`), 0o600); errWrite != nil {
		t.Fatalf("failed to write auth file %s: %v", name, errWrite)
	}
}

func listAuthFilesForStatusCodeTest(t *testing.T, h *Handler) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	return payload.Files
}
