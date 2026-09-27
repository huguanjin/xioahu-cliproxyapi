package executor

import (
	"net/http"
	"testing"
)

// tosViolationBody is the payload Antigravity returns for an account banned for
// a terms-of-service violation. The reason is the only thing that separates it
// from a validation 403, so it is the field under test.
const tosViolationBody = `{
  "error": {
    "code": 403,
    "message": "This account has been suspended for a terms of service violation.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "TOS_VIOLATION",
        "domain": "cloudcode-pa.googleapis.com"
      }
    ]
  }
}`

// plainForbiddenBody is a 403 that names no subtype: a project permission
// problem, not an account verdict. It must not be labelled either way.
const plainForbiddenBody = `{"error":{"code":403,"message":"Permission denied."}}`

// validationURLBody is the real shape: the ErrorInfo carries no URL, the link
// lives in a sibling metadata block.
const validationURLBody = `{
  "error": {
    "code": 403,
    "message": "Verify your account to continue.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "VALIDATION_REQUIRED",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "validation_url": "https://accounts.google.com/signin/continue?verify=abc123"
        }
      }
    ]
  }
}`

func TestAntigravityClassifyForbiddenBody(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantType      string
		wantURL       string
		wantURLNotSet bool
	}{
		{
			name:     "validation required from ErrorInfo",
			body:     validationRequiredBody,
			wantType: antigravityForbiddenTypeValidation,
		},
		{
			name:     "tos violation from ErrorInfo",
			body:     tosViolationBody,
			wantType: antigravityForbiddenTypeViolation,
		},
		{
			// A plain 403 stays unclassified. Guessing here would let a
			// project-level permission error read as an account ban.
			name:     "plain 403 names no subtype",
			body:     plainForbiddenBody,
			wantType: "",
		},
		{
			name:     "empty body",
			body:     "",
			wantType: "",
		},
		{
			// Another Google surface emitting the same reason is not this one.
			name:     "wrong domain is not read",
			body:     `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION","domain":"other.googleapis.com"}]}}`,
			wantType: "",
		},
		{
			name:     "reason matched case insensitively",
			body:     `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"tos_violation","domain":"CLOUDCODE-PA.GOOGLEAPIS.COM"}]}}`,
			wantType: antigravityForbiddenTypeViolation,
		},
		{
			// The fallback only runs when no structured reason exists.
			name:     "free text validation marker with no ErrorInfo",
			body:     `{"error":{"code":403,"message":"VALIDATION_REQUIRED"}}`,
			wantType: antigravityForbiddenTypeValidation,
		},
		{
			name:     "free text terms of service with no ErrorInfo",
			body:     `{"error":{"code":403,"message":"Account suspended: terms of service"}}`,
			wantType: antigravityForbiddenTypeViolation,
		},
		{
			// A structured reason wins: the body merely mentioning the word
			// "violation" must not override what the ErrorInfo actually says.
			name:     "structured reason beats a lookalike substring",
			body:     `{"error":{"message":"no violation found","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","domain":"cloudcode-pa.googleapis.com"}]}}`,
			wantType: antigravityForbiddenTypeValidation,
		},
		{
			name:     "validation url extracted from metadata",
			body:     validationURLBody,
			wantType: antigravityForbiddenTypeValidation,
			wantURL:  "https://accounts.google.com/signin/continue?verify=abc123",
		},
		{
			// A violation has nothing to verify, so no link should come back even
			// if the body happens to contain one.
			name:          "violation carries no validation url",
			body:          tosViolationBody,
			wantType:      antigravityForbiddenTypeViolation,
			wantURLNotSet: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotURL := antigravityClassifyForbiddenBody([]byte(tc.body))
			if gotType != tc.wantType {
				t.Fatalf("forbidden type = %q, want %q", gotType, tc.wantType)
			}
			if tc.wantURLNotSet {
				if gotURL != "" {
					t.Fatalf("validation url = %q, want empty", gotURL)
				}
				return
			}
			if tc.wantURL != "" && gotURL != tc.wantURL {
				t.Fatalf("validation url = %q, want %q", gotURL, tc.wantURL)
			}
		})
	}
}

// TestAntigravitySelfTestResultCarriesForbiddenType pins the wiring between the
// classifier and the probe's result: a 403 has to leave SelfTestCredential with
// its subtype attached, or the caller has nothing to split on.
func TestAntigravitySelfTestResultCarriesForbiddenType(t *testing.T) {
	body := []byte(validationURLBody)
	forbiddenType, validationURL := antigravityClassifyForbiddenBody(body)
	if forbiddenType != antigravityForbiddenTypeValidation {
		t.Fatalf("forbidden type = %q, want %q", forbiddenType, antigravityForbiddenTypeValidation)
	}
	if validationURL == "" {
		t.Fatal("a validation 403 must carry its verification link through")
	}

	// The same body must keep classifying as a credential-action failure on the
	// request path, so the probe and a live 403 agree.
	statusErrValue := newAntigravityStatusErr(http.StatusForbidden, body)
	if !statusErrValue.CredentialNeedsAction() {
		t.Fatal("validation 403 must still request credential action on the request path")
	}
}
