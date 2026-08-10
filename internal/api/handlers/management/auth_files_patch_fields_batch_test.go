package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestBatchPatchAuthFileFields_AllSucceed(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, name := range []string{"a.json", "b.json"} {
		record := &coreauth.Auth{
			ID:         name,
			FileName:   name,
			Provider:   "claude",
			Attributes: map[string]string{"path": "/tmp/" + name},
			Metadata:   map[string]any{"type": "claude"},
		}
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register auth record %s: %v", name, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"names":["a.json","b.json"],"fields":{"proxy_url":"socks5://user:pass@1.2.3.4:1080"}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.BatchPatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	for _, name := range []string{"a.json", "b.json"} {
		updated, ok := manager.GetByID(name)
		if !ok {
			t.Fatalf("expected %s to exist", name)
		}
		if updated.ProxyURL != "socks5://user:pass@1.2.3.4:1080" {
			t.Fatalf("%s proxy_url = %q, want socks5://user:pass@1.2.3.4:1080", name, updated.ProxyURL)
		}
	}
}

func TestBatchPatchAuthFileFields_PartialFailureReturns207(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "exists.json",
		FileName:   "exists.json",
		Provider:   "claude",
		Attributes: map[string]string{"path": "/tmp/exists.json"},
		Metadata:   map[string]any{"type": "claude"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"names":["exists.json","missing.json"],"fields":{"proxy_url":"socks5://1.2.3.4:1080"}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.BatchPatchAuthFileFields(ctx)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusMultiStatus, rec.Body.String())
	}

	var payload struct {
		Status  string           `json:"status"`
		Updated int              `json:"updated"`
		Files   []string         `json:"files"`
		Failed  []map[string]any `json:"failed"`
	}
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode response: %v", errDecode)
	}
	if payload.Status != "partial" || payload.Updated != 1 || len(payload.Files) != 1 || payload.Files[0] != "exists.json" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	if len(payload.Failed) != 1 || payload.Failed[0]["name"] != "missing.json" {
		t.Fatalf("expected missing.json in failed list, got %+v", payload.Failed)
	}

	updated, ok := manager.GetByID("exists.json")
	if !ok || updated.ProxyURL != "socks5://1.2.3.4:1080" {
		t.Fatalf("expected exists.json to be updated, got %+v", updated)
	}
}

func TestBatchPatchAuthFileFields_AllNotFoundReturns400(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"names":["missing1.json","missing2.json"],"fields":{"proxy_url":"socks5://1.2.3.4:1080"}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.BatchPatchAuthFileFields(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestBatchPatchAuthFileFields_EmptyFieldsReturns400(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	record := &coreauth.Auth{
		ID:         "a.json",
		FileName:   "a.json",
		Provider:   "claude",
		Attributes: map[string]string{"path": "/tmp/a.json"},
		Metadata:   map[string]any{"type": "claude"},
	}
	if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"names":["a.json"],"fields":{}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.BatchPatchAuthFileFields(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestBatchPatchAuthFileFields_EmptyNamesReturns400(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"names":[],"fields":{"proxy_url":"socks5://1.2.3.4:1080"}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.BatchPatchAuthFileFields(ctx)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestBatchPatchAuthFileFields_NameAndNamesMerged(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	store := &memoryAuthStore{}
	manager := coreauth.NewManager(store, nil, nil)
	for _, name := range []string{"a.json", "b.json"} {
		record := &coreauth.Auth{
			ID:         name,
			FileName:   name,
			Provider:   "claude",
			Attributes: map[string]string{"path": "/tmp/" + name},
			Metadata:   map[string]any{"type": "claude"},
		}
		if _, errRegister := manager.Register(context.Background(), record); errRegister != nil {
			t.Fatalf("failed to register auth record %s: %v", name, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	body := `{"name":"a.json","names":["b.json"],"fields":{"note":"batched"}}`
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/fields/batch", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.BatchPatchAuthFileFields(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	for _, name := range []string{"a.json", "b.json"} {
		updated, ok := manager.GetByID(name)
		if !ok || updated.Attributes["note"] != "batched" {
			t.Fatalf("%s note attribute = %+v, want batched", name, updated)
		}
	}
}
