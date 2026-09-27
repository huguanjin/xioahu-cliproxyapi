package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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

// The deep probe's body must be in the v1internal envelope. Antigravity looks for
// contents and generationConfig one level down, under "request", and answers an
// unwrapped body with INVALID_ARGUMENT ("Unknown name contents") — a 400 that then
// read as a transient upstream fault, so the probe never actually ran and reported
// nothing. Asserting only on substrings is what let that ship: the strings were all
// present, in the wrong place. This test parses the JSON and checks the structure.
func TestAntigravitySelfTestGenerationBodyUsesV1InternalEnvelope(t *testing.T) {
	body := antigravitySelfTestGenerationBody()

	var decoded map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil {
		t.Fatalf("generation body is not valid JSON: %v (%s)", errUnmarshal, body)
	}

	// The envelope is what the upstream validates first, so its absence is checked
	// separately from the fields inside it.
	rawRequest, ok := decoded["request"]
	if !ok {
		t.Fatalf("generation body must nest the payload under \"request\"; got top-level keys %v", keysOf(decoded))
	}
	// Nothing may sit beside "request": a stray top-level "contents" is exactly the
	// shape the upstream rejected, and would fail again even with the envelope added.
	for key := range decoded {
		if key != "request" {
			t.Fatalf("unexpected top-level key %q; the probe payload belongs under \"request\"", key)
		}
	}

	var inner map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(rawRequest, &inner); errUnmarshal != nil {
		t.Fatalf("\"request\" is not an object: %v", errUnmarshal)
	}
	for _, field := range []string{"contents", "generationConfig"} {
		if _, ok := inner[field]; !ok {
			t.Fatalf("\"request\" must carry %q; got keys %v", field, keysOf(inner))
		}
	}

	// And the probe must still be a single short turn, not something a model would
	// spend real budget on.
	var contents []struct {
		Role  string `json:"role"`
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	if errUnmarshal := json.Unmarshal(inner["contents"], &contents); errUnmarshal != nil {
		t.Fatalf("contents is not an array of turns: %v", errUnmarshal)
	}
	if len(contents) != 1 || len(contents[0].Parts) != 1 {
		t.Fatalf("the probe must be exactly one turn with one part, got %+v", contents)
	}
	if contents[0].Role != "user" {
		t.Fatalf("the probe turn must be from the user, got %q", contents[0].Role)
	}
	if contents[0].Parts[0].Text != antigravitySelfTestGenerationText {
		t.Fatalf("the probe prompt must be the fixed text %q, got %q",
			antigravitySelfTestGenerationText, contents[0].Parts[0].Text)
	}

	var genConfig struct {
		MaxOutputTokens int `json:"maxOutputTokens"`
	}
	if errUnmarshal := json.Unmarshal(inner["generationConfig"], &genConfig); errUnmarshal != nil {
		t.Fatalf("generationConfig is not an object: %v", errUnmarshal)
	}
	if genConfig.MaxOutputTokens != antigravitySelfTestMaxOutputTokens {
		t.Fatalf("maxOutputTokens = %d, want %d", genConfig.MaxOutputTokens, antigravitySelfTestMaxOutputTokens)
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
