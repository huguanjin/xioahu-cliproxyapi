package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// Antigravity 403 subtypes. The upstream sends every rejection as a bare 403, so
// the body is the only thing that separates an account whose owner can fix it
// from one that is gone. They are carried out to the caller as the failure kind
// so a cooldown decision is never made on a status code alone.
const (
	antigravityForbiddenTypeValidation = "validation"
	antigravityForbiddenTypeViolation  = "violation"
	// antigravityForbiddenTypeRestricted is a separate ban from a terms-of-service
	// violation: the upstream reports it as an OAuth-style access_denied with
	// "Account Restricted" rather than a ToS message. Both are unusable without
	// operator action, but they are different upstream states and an appeal goes
	// to a different place, so they must not be merged into one bucket.
	antigravityForbiddenTypeRestricted = "restricted"
)

// antigravityValidationURLPattern is the last-resort extraction when the 403
// body carries a link outside the documented metadata shape.
var antigravityValidationURLPattern = regexp.MustCompile(`https://[^\s"'\\]+`)

// antigravityClassifyForbiddenBody splits a 403 body into its subtype and, when
// the account is merely unverified, the link the operator has to open.
//
// The subtype comes from the structured Google rpc ErrorInfo first, and only
// falls back to free text when no ErrorInfo is present. That ordering is
// deliberate: a substring match on "violation" would label any body that merely
// mentions the word, and a false "violation" reads as a ban that is not there.
func antigravityClassifyForbiddenBody(body []byte) (string, string) {
	if len(body) == 0 {
		return "", ""
	}
	reason, structured := antigravityForbiddenErrorReason(body)
	forbiddenType := ""
	if structured {
		// A structured reason is authoritative. When one is present but names
		// neither subtype — including a reason aimed at another surface, whose
		// domain the helper refuses to read — the body stays unclassified rather
		// than being re-matched through free text. The reason identifier itself
		// lives in the body, so a substring fallback would otherwise match the
		// very field the domain guard exists to reject.
		switch strings.ToUpper(reason) {
		case "VALIDATION_REQUIRED", "VALIDATION_FAILED":
			forbiddenType = antigravityForbiddenTypeValidation
		case "TOS_VIOLATION", "TERMS_OF_SERVICE_VIOLATION":
			forbiddenType = antigravityForbiddenTypeViolation
		}
	} else {
		lower := strings.ToLower(string(body))
		switch {
		case strings.Contains(lower, "validation_required"):
			forbiddenType = antigravityForbiddenTypeValidation
		case strings.Contains(lower, "terms of service") || strings.Contains(lower, "tos_violation"):
			forbiddenType = antigravityForbiddenTypeViolation
		// The restricted-account rejection arrives as a flat OAuth error object
		// rather than the nested rpc ErrorInfo the other two use, which is why it
		// reaches this branch at all. "access_denied" alone is the OAuth code for
		// any refusal, so the description has to confirm it is an account
		// restriction before the body is labelled: calling an ordinary refusal a
		// ban would be the more expensive mistake.
		case strings.Contains(lower, "account restricted") ||
			strings.Contains(lower, "account_restricted") ||
			(strings.Contains(lower, "access_denied") && strings.Contains(lower, "restrict")):
			forbiddenType = antigravityForbiddenTypeRestricted
		}
	}
	if forbiddenType != antigravityForbiddenTypeValidation {
		return forbiddenType, ""
	}
	return forbiddenType, antigravityExtractValidationURL(body)
}

// antigravityForbiddenErrorReason returns the reason of the first ErrorInfo
// detail in the body and whether any such detail was present at all. The bool is
// what separates "no structured verdict to read" from "a structured verdict the
// domain check refused": the caller must not fall back to free text in the
// second case, or a wrong-surface reason would be matched by its own text.
//
// A detail whose domain is not the Antigravity surface yields the empty reason
// with structured true, so it is deliberately not read.
func antigravityForbiddenErrorReason(body []byte) (string, bool) {
	details := gjson.GetBytes(body, "error.details")
	if !details.Exists() || !details.IsArray() {
		return "", false
	}
	found := false
	for _, detail := range details.Array() {
		if detail.Get("@type").String() != "type.googleapis.com/google.rpc.ErrorInfo" {
			continue
		}
		found = true
		if !strings.EqualFold(strings.TrimSpace(detail.Get("domain").String()), "cloudcode-pa.googleapis.com") {
			continue
		}
		return strings.TrimSpace(detail.Get("reason").String()), true
	}
	return "", found
}

// antigravityExtractValidationURL pulls the verification or appeal link out of a
// validation 403, so the operator can act on it directly instead of pasting the
// whole body into a browser search.
func antigravityExtractValidationURL(body []byte) string {
	var parsed struct {
		Error struct {
			Details []struct {
				Metadata map[string]string `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &parsed); errUnmarshal == nil {
		for _, detail := range parsed.Error.Details {
			if url := strings.TrimSpace(detail.Metadata["validation_url"]); url != "" {
				return url
			}
			if url := strings.TrimSpace(detail.Metadata["appeal_url"]); url != "" {
				return url
			}
		}
	}
	lower := strings.ToLower(string(body))
	if !strings.Contains(lower, "validation") &&
		!strings.Contains(lower, "verify") &&
		!strings.Contains(lower, "appeal") {
		return ""
	}
	return strings.TrimSpace(antigravityValidationURLPattern.FindString(string(body)))
}

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
	// A 403 is the only place the upstream tells the difference between an
	// account its owner can still rescue and one that is gone, so the subtype
	// travels with the result instead of being flattened into the status code.
	if lastStatus == http.StatusForbidden {
		result.ForbiddenType, result.ValidationURL = antigravityClassifyForbiddenBody(lastBody)
	}
	if lastStatus == http.StatusTooManyRequests {
		if retryAfter, parseErr := helps.ParseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
			result.RetryAfter = retryAfter
		}
	}
	return result, nil
}
