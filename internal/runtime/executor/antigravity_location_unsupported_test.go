package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const antigravityLocationUnsupportedBody = `{
  "error": {
    "code": 400,
    "message": "User location is not supported for the API use.",
    "status": "FAILED_PRECONDITION"
  }
}`

// The conductor tells the region rejection apart from an ordinary request fault
// by reading the error text alone (sdk/cliproxy/auth isLocationUnsupportedError),
// so the executor has to hand the upstream body over verbatim with the HTTP status
// attached. Wrapping or re-encoding it here would silently turn the credential
// rotation back off, because the classification would stop matching.
func TestAntigravityExecutorPassesLocationRejectionThroughVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(antigravityLocationUnsupportedBody))
	}))
	defer server.Close()

	exec := NewAntigravityExecutor(&config.Config{RequestRetry: 1})
	newAuth := func() *cliproxyauth.Auth {
		return &cliproxyauth.Auth{
			ID:       "auth-location-unsupported",
			Provider: "antigravity",
			Attributes: map[string]string{
				"base_url": server.URL,
			},
			Metadata: map[string]any{
				"access_token": "token",
				"project_id":   "project-1",
				"expired":      time.Now().Add(time.Hour).Format(time.RFC3339),
			},
		}
	}
	request := cliproxyexecutor.Request{
		Model:   "gemini-3.5-flash-low",
		Payload: []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}

	assertVerbatim := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected the upstream 400 to surface as an error")
		}
		var statusCoder interface{ StatusCode() int }
		if !errors.As(err, &statusCoder) || statusCoder.StatusCode() != http.StatusBadRequest {
			t.Fatalf("error %v does not carry status %d", err, http.StatusBadRequest)
		}
		if got := err.Error(); got != antigravityLocationUnsupportedBody {
			t.Fatalf("error text = %q, want the upstream body verbatim", got)
		}
	}

	t.Run("non-stream", func(t *testing.T) {
		_, err := exec.Execute(context.Background(), newAuth(), request, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini,
			Stream:       false,
		})
		assertVerbatim(t, err)
	})

	t.Run("stream", func(t *testing.T) {
		result, err := exec.ExecuteStream(context.Background(), newAuth(), request, cliproxyexecutor.Options{
			SourceFormat: sdktranslator.FormatGemini,
			Stream:       true,
		})
		if result != nil {
			for range result.Chunks {
			}
		}
		assertVerbatim(t, err)
	})
}
