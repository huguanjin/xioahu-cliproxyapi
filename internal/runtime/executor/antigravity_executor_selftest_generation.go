package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// antigravitySelfTestGenerationText is the whole prompt the deep probe sends. One
// short turn in, one short turn out: the probe needs the upstream to decide
// whether this credential may generate, not to produce anything useful. A plain
// greeting is the smallest prompt a text model answers without preamble.
const antigravitySelfTestGenerationText = "你好"

// antigravitySelfTestMaxOutputTokens keeps the deep probe's sampled cost small.
// Some models reject an unset limit, so it is sent rather than omitted.
//
// For Gemini-family models the live request path strips this field before sending
// (antigravity_executor_request.go), so the cap is best-effort there and the probe
// relies on the prompt being short instead. Claude models keep it.
const antigravitySelfTestMaxOutputTokens = 1

// SelfTestCredentialGeneration performs the deep probe: a minimal but real
// generateContent call.
//
// It exists because the cheap countTokens probe cannot see an exhausted quota.
// A credential whose window is spent keeps a valid token and a valid
// authorization chain, so countTokens answers 2xx while every generation the
// dispatcher routes to it is refused — the exact credential a probe would most
// like to catch, and the one a cheap probe reliably misses.
//
// The request is built through the same path a live call takes — envelope
// construction, project binding, schema sanitisation, signature replay — so a
// credential that fails here fails for the same reason it would fail serving
// traffic. That is the point: a probe that diverges from the real path can pass a
// credential that live traffic then rejects.
//
// It is deliberately never retried across base URLs for a 429. The rung it sits
// on is "is this credential's generation quota spent", and a rate limit is that
// answer; retrying would only get the same answer from another region.
func (e *AntigravityExecutor) SelfTestCredentialGeneration(ctx context.Context, auth *cliproxyauth.Auth, model string) (*cliproxyauth.CredentialSelfTestResult, error) {
	if auth == nil {
		return nil, errors.New("antigravity self-test: auth is nil")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, errors.New("antigravity self-test: no generation model configured")
	}
	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}
	if strings.TrimSpace(token) == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "antigravity self-test: missing access token"}
	}

	payload := antigravitySelfTestGenerationBody()

	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)

	var last *cliproxyauth.CredentialSelfTestResult
	for idx, baseURL := range baseURLs {
		httpReq, errBuild := e.buildRequest(ctx, auth, token, model, payload, false, "", baseURL)
		if errBuild != nil {
			return nil, errBuild
		}
		util.ApplyCustomHeadersFromAttrs(httpReq, auth.Attributes)

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errDo)
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return nil, errDo
			}
			if idx+1 < len(baseURLs) {
				continue
			}
			return nil, errDo
		}

		// The request has an unread body; drain and close it so the connection can
		// be reused, and so a base-URL retry does not leak it.
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity self-test: close response body error: %v", errClose)
		}
		if errRead != nil {
			return nil, errRead
		}

		result := &cliproxyauth.CredentialSelfTestResult{
			AuthID:     auth.ID,
			Provider:   e.Identifier(),
			StatusCode: httpResp.StatusCode,
			Tier:       cliproxyauth.SelfTestTierGeneration,
		}
		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			return result, nil
		}
		result.Message = string(bodyBytes)
		if httpResp.StatusCode == http.StatusForbidden {
			result.ForbiddenType, result.ValidationURL = antigravityClassifyForbiddenBody(bodyBytes)
		}
		if httpResp.StatusCode == http.StatusTooManyRequests {
			if retryAfter, parseErr := helps.ParseRetryDelay(bodyBytes); parseErr == nil && retryAfter != nil {
				result.RetryAfter = retryAfter
			}
		}
		last = result
		// Only another region can change the answer for a per-region rate limit;
		// for a spent quota it will not, but one retry is cheap and the credential
		// may simply be pinned to a region that is throttled.
		if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
			continue
		}
		break
	}

	if last == nil {
		return nil, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity self-test: no base url available"}
	}
	return last, nil
}

// antigravitySelfTestGenerationBody is the deep probe's request body: one token
// in, one token out.
//
// The payload has to sit inside a "request" envelope. Antigravity's generateContent
// takes a v1internal-shaped body — the same wrapper buildRequest produces for a
// live call — and an unwrapped {contents, generationConfig} is rejected with
// INVALID_ARGUMENT ("Unknown name contents"), because the upstream looks for both
// fields one level down. Getting this wrong made every deep probe fail with a 400
// that then read as a transient upstream fault, so the probe silently never ran.
//
// Deep probing a Claude model is the same shape: Antigravity routes Claude models
// through this one endpoint and only the model field differs.
func antigravitySelfTestGenerationBody() []byte {
	return []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"` +
		antigravitySelfTestGenerationText +
		`"}]}],"generationConfig":{"maxOutputTokens":` +
		strconv.Itoa(antigravitySelfTestMaxOutputTokens) + `}}}`)
}
