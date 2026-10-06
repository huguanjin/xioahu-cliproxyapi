package management

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

const locationUnsupportedTestBody = `{
  "error": {
    "code": 400,
    "message": "User location is not supported for the API use.",
    "status": "FAILED_PRECONDITION"
  }
}`

// locationRejection mimics the error the Antigravity executor returns for the
// upstream region rejection: the raw body as the text, the HTTP status attached.
type locationRejection struct{}

func (locationRejection) Error() string   { return locationUnsupportedTestBody }
func (locationRejection) StatusCode() int { return http.StatusBadRequest }

// locationTestExecutor answers the region rejection for the listed credentials and
// succeeds for the rest. It is the smallest executor the conductor will route to.
type locationTestExecutor struct {
	blocked map[string]struct{}
}

func (e *locationTestExecutor) Identifier() string { return "antigravity" }

func (e *locationTestExecutor) Execute(_ context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	if _, blocked := e.blocked[auth.ID]; blocked {
		return cliproxyexecutor.Response{}, locationRejection{}
	}
	return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
}

func (e *locationTestExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("streaming is not used by this test")
}

func (e *locationTestExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *locationTestExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("token counting is not used by this test")
}

func (e *locationTestExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

// This pins the contract the management UI's "region restricted" filter relies on:
// after a request is routed around a region-restricted credential, the credential
// list carries the machine-readable marker the UI filters on, and the credential
// that served the request carries nothing.
func TestListAuthFilesFlagsRegionRestrictedCredential(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	const (
		model     = "gemini-3.8-flash-high"
		blockedID = "aa-region-blocked"
		goodID    = "bb-region-good"
	)
	authDir := t.TempDir()
	writeAuthFileForStatusCodeTest(t, authDir, "blocked.json")
	writeAuthFileForStatusCodeTest(t, authDir, "good.json")

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&locationTestExecutor{blocked: map[string]struct{}{blockedID: {}}})
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:         blockedID,
		FileName:   "blocked.json",
		Provider:   "antigravity",
		Attributes: map[string]string{"path": filepath.Join(authDir, "blocked.json")},
	})
	registerAuthForLookupTest(t, manager, &coreauth.Auth{
		ID:         goodID,
		FileName:   "good.json",
		Provider:   "antigravity",
		Attributes: map[string]string{"path": filepath.Join(authDir, "good.json")},
	})
	modelsFor(t, blockedID, model)
	modelsFor(t, goodID, model)

	resp, errExecute := manager.Execute(context.Background(), []string{"antigravity"}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{})
	if errExecute != nil {
		t.Fatalf("expected the request to be routed around the restricted credential, got %v", errExecute)
	}
	if got := string(resp.Payload); got != goodID {
		t.Fatalf("served by %q, want %q", got, goodID)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	files := listAuthFilesForStatusCodeTest(t, h)
	byID := make(map[string]map[string]any, len(files))
	for _, file := range files {
		id, _ := file["id"].(string)
		byID[id] = file
	}

	blocked, ok := byID[blockedID]
	if !ok {
		t.Fatalf("missing %s in %#v", blockedID, files)
	}
	// JSON numbers decode as float64.
	if code, _ := blocked["last_status_code"].(float64); code != http.StatusBadRequest {
		t.Fatalf("last_status_code = %#v, want 400", blocked["last_status_code"])
	}
	if code, _ := blocked["last_error_code"].(string); code != "location_unsupported" {
		t.Fatalf("last_error_code = %#v, want location_unsupported", blocked["last_error_code"])
	}
	if message, _ := blocked["status_message"].(string); message != "location_unsupported" {
		t.Fatalf("status_message = %#v, want location_unsupported", blocked["status_message"])
	}
	if disabled, _ := blocked["disabled"].(bool); disabled {
		t.Fatal("a region-restricted credential must stay enabled")
	}

	models, _ := blocked["blocked_models"].([]any)
	if len(models) != 1 {
		t.Fatalf("blocked_models = %#v, want exactly the cooling model", blocked["blocked_models"])
	}
	entry, _ := models[0].(map[string]any)
	if id, _ := entry["id"].(string); id != model {
		t.Fatalf("blocked model id = %#v, want %q", entry["id"], model)
	}
	if message, _ := entry["status_message"].(string); message != "location_unsupported" {
		t.Fatalf("blocked model status_message = %#v, want location_unsupported", entry["status_message"])
	}
	// A fixed cooldown has a known end, which the card can show.
	if retryAt, _ := entry["retry_at"].(string); retryAt == "" {
		t.Fatal("expected retry_at for a cooldown with a known expiry")
	}

	good, ok := byID[goodID]
	if !ok {
		t.Fatalf("missing %s in %#v", goodID, files)
	}
	for _, key := range []string{"last_status_code", "last_error_code", "blocked_models"} {
		if _, present := good[key]; present {
			t.Fatalf("%s = %#v on the credential that served the request, want the field absent", key, good[key])
		}
	}
}
