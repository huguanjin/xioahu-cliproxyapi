package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// antigravitySelfTestPayload is the smallest body countTokens accepts. A probe
// only needs the upstream to decide whether this credential may call, so a
// one-token prompt is enough and keeps the sampled cost at zero. The probe
// carries no model: the real countTokens request identifies it in the payload
// pipeline, and a rejection here is about the credential, not the model.
const antigravitySelfTestPayload = `{"request":{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}}`

// SelfTestCredential probes one credential against the countTokens endpoint.
//
// countTokens goes through the same inference authorization chain a real
// generation does, so a credential the upstream has rejected answers 403 here
// too — unlike the quota-class endpoints, which can stay healthy while
// generation is refused. It also consumes no generation quota, which is what
// makes probing on a timer acceptable.
//
// A returned error means the probe could not be performed, not that the
// credential is bad; callers must not cool a credential down on it. A nil
// result likewise carries no verdict when the upstream was merely unavailable.
//
// The probe deliberately skips the payload translation pipeline a real
// countTokens call runs. It needs the authorization verdict, not a token count:
// a rejected credential is refused before the body is parsed, so a minimal
// payload reaches the same answer, and any body-level complaint comes back as a
// 400 the caller ignores anyway.
func (e *AntigravityExecutor) SelfTestCredential(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.CredentialSelfTestResult, error) {
	if auth == nil {
		return nil, errors.New("antigravity self-test: auth is nil")
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

	payload := []byte(antigravitySelfTestPayload)
	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newAntigravityHTTPClient(ctx, e.cfg, auth, 0)

	authID := auth.ID
	authLabel := auth.Label
	authType, authValue := auth.AccountInfo()

	var lastStatus int
	var lastBody []byte

	for idx, baseURL := range baseURLs {
		base := strings.TrimSuffix(baseURL, "/")
		if base == "" {
			base = buildBaseURL(auth)
		}

		requestURL := base + antigravityCountTokensPath
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(payload))
		if errReq != nil {
			return nil, errReq
		}
		httpReq.Close = true
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveUserAgent(auth))
		if host := resolveHost(base); host != "" {
			httpReq.Host = host
		}
		util.ApplyCustomHeadersFromAttrs(httpReq, auth.Attributes)

		helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
			URL:       requestURL,
			Method:    http.MethodPost,
			Headers:   httpReq.Header.Clone(),
			Body:      payload,
			Provider:  e.Identifier(),
			AuthID:    authID,
			AuthLabel: authLabel,
			AuthType:  authType,
			AuthValue: authValue,
		})

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errDo)
			// A cancelled or expired probe is our own deadline, not the
			// upstream's opinion; retrying another base url would only burn
			// the remaining budget.
			if errors.Is(errDo, context.Canceled) || errors.Is(errDo, context.DeadlineExceeded) {
				return nil, errDo
			}
			lastStatus = 0
			lastBody = nil
			if idx+1 < len(baseURLs) {
				log.Debugf("antigravity self-test: request error on base url %s, retrying with fallback: %s", baseURL, baseURLs[idx+1])
				continue
			}
			return nil, errDo
		}

		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("antigravity self-test: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)

		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			return &cliproxyauth.CredentialSelfTestResult{
				AuthID:     authID,
				Provider:   e.Identifier(),
				StatusCode: httpResp.StatusCode,
			}, nil
		}

		lastStatus = httpResp.StatusCode
		lastBody = append([]byte(nil), bodyBytes...)
		// A rate limit may be per-region, so another base url can still answer
		// for this credential; anything else is the credential's own verdict.
		if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
			log.Debugf("antigravity self-test: rate limited on base url %s, retrying with fallback: %s", baseURL, baseURLs[idx+1])
			continue
		}
		break
	}

	if lastStatus == 0 {
		return nil, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity self-test: no base url available"}
	}
	result := &cliproxyauth.CredentialSelfTestResult{
		AuthID:     authID,
		Provider:   e.Identifier(),
		StatusCode: lastStatus,
		Message:    string(lastBody),
	}
	if lastStatus == http.StatusTooManyRequests {
		if retryAfter, parseErr := helps.ParseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
			result.RetryAfter = retryAfter
		}
	}
	return result, nil
}
