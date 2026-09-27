package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// A rejection must come back as a result carrying the upstream status, not as an
// error: an error means the probe could not run, and callers treat that as no
// verdict on the credential.
func TestAntigravitySelfTestCredentialReportsForbidden(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"status":"PERMISSION_DENIED"}}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	result, errProbe := exec.SelfTestCredential(context.Background(), testAntigravityAuth(server.URL))
	if errProbe != nil {
		t.Fatalf("SelfTestCredential() error = %v", errProbe)
	}
	if result == nil {
		t.Fatal("SelfTestCredential() returned no result")
	}
	if gotPath != antigravityCountTokensPath {
		t.Fatalf("probe path = %q, want %q", gotPath, antigravityCountTokensPath)
	}
	if result.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusForbidden)
	}
	if result.Message == "" {
		t.Fatal("expected the upstream body to be carried on a rejection")
	}
	if result.Provider != exec.Identifier() {
		t.Fatalf("provider = %q, want %q", result.Provider, exec.Identifier())
	}
}

// A healthy credential must report its 2xx so the caller can leave scheduling
// state alone; the probe itself draws no conclusion from success.
func TestAntigravitySelfTestCredentialReportsSuccess(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"totalTokens":1}`))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	result, errProbe := exec.SelfTestCredential(context.Background(), testAntigravityAuth(server.URL))
	if errProbe != nil {
		t.Fatalf("SelfTestCredential() error = %v", errProbe)
	}
	if result == nil || result.StatusCode != http.StatusOK {
		t.Fatalf("expected a 200 result, got %+v", result)
	}
	if authHeader != "Bearer token-123" {
		t.Fatalf("Authorization = %q, want the credential's bearer token", authHeader)
	}
}

// A missing credential cannot be probed at all, so it must surface as an error
// rather than a verdict that would cool a credential down.
func TestAntigravitySelfTestCredentialErrorsWithoutToken(t *testing.T) {
	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	auth := &cliproxyauth.Auth{
		Attributes: map[string]string{"base_url": "http://127.0.0.1:1"},
		Metadata:   map[string]any{"access_token": ""},
	}
	if _, errProbe := exec.SelfTestCredential(context.Background(), auth); errProbe == nil {
		t.Fatal("expected an error when the credential has no usable access token")
	}
}

// An expired probe budget is our own deadline, not the upstream's opinion: the
// probe must stop and report an error instead of retrying the fallback bases.
func TestAntigravitySelfTestCredentialHonoursDeadline(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, errProbe := exec.SelfTestCredential(ctx, testAntigravityAuth(server.URL)); errProbe == nil {
		t.Fatal("expected an error when the probe deadline expires")
	}
	if attempts != 1 {
		t.Fatalf("expected the probe to stop after the deadline, got %d attempts", attempts)
	}
}

// The deep probe's body is what makes it cheap enough to run on a schedule, so
// its shape is part of the contract: one token out, and the prompt is constant so
// the probe cannot be mistaken for user traffic.
func TestAntigravitySelfTestGenerationBodyIsMinimal(t *testing.T) {
	body := antigravitySelfTestGenerationBody()
	if len(body) == 0 {
		t.Fatal("generation body is empty")
	}
	text := string(body)
	if !strings.Contains(text, `"maxOutputTokens":1`) {
		t.Fatalf("generation body must cap output at one token, got %s", text)
	}
	if !strings.Contains(text, antigravitySelfTestGenerationText) {
		t.Fatalf("generation body must carry the fixed probe prompt, got %s", text)
	}
	if !strings.Contains(text, `"role":"user"`) {
		t.Fatalf("generation body must be a well-formed single-turn request, got %s", text)
	}
	// A probe that grew a schema or a system instruction would stop being a probe.
	if strings.Contains(text, "systemInstruction") || strings.Contains(text, "responseSchema") {
		t.Fatalf("generation body must stay minimal, got %s", text)
	}
}
