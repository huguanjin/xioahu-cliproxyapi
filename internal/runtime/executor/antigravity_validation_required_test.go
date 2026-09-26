package executor

import (
	"net/http"
	"testing"
)

// validationRequiredBody is the payload Antigravity returns for an account that
// still has to pass Google's account verification step.
const validationRequiredBody = `{
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
          "validation_url_link_text": "Verify your account",
          "validation_error_message": "Verify your account to continue."
        }
      },
      {
        "@type": "type.googleapis.com/google.rpc.Help",
        "links": []
      }
    ]
  }
}`

func TestAntigravityHasValidationRequiredReason(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "real verification payload", body: validationRequiredBody, want: true},
		{name: "empty body", body: "", want: false},
		{name: "non json body", body: "Verify your account to continue.", want: false},
		{name: "json without details", body: `{"error":{"code":403,"message":"Verify your account to continue."}}`, want: false},
		{
			// Another Google surface emitting the same reason must not disable
			// an Antigravity credential.
			name: "wrong domain",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","domain":"other.googleapis.com"}]}}`,
			want: false,
		},
		{
			name: "missing domain",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED"}]}}`,
			want: false,
		},
		{
			name: "different reason on the antigravity domain",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SOMETHING_ELSE","domain":"cloudcode-pa.googleapis.com"}]}}`,
			want: false,
		},
		{
			// A Help detail carrying a lookalike field must not be mistaken for
			// an ErrorInfo.
			name: "reason only on a non ErrorInfo detail",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.Help","reason":"VALIDATION_REQUIRED","domain":"cloudcode-pa.googleapis.com"}]}}`,
			want: false,
		},
		{
			name: "reason matched deep in the array, case insensitively",
			body: `{"error":{"details":[{"@type":"type.googleapis.com/google.rpc.Help"},{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"validation_required","domain":"CLOUDCODE-PA.GOOGLEAPIS.COM"}]}}`,
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityHasValidationRequiredReason([]byte(tc.body)); got != tc.want {
				t.Fatalf("antigravityHasValidationRequiredReason() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewAntigravityStatusErrFlagsCredentialAction(t *testing.T) {
	verified := newAntigravityStatusErr(http.StatusForbidden, []byte(validationRequiredBody))
	if verified.code != http.StatusForbidden {
		t.Fatalf("code = %d, want %d", verified.code, http.StatusForbidden)
	}
	if !verified.CredentialNeedsAction() {
		t.Fatal("verification failure must request credential action")
	}

	// A plain 403 (e.g. a project permission problem) is transient as far as the
	// credential is concerned and must keep the normal cooldown path.
	plain := newAntigravityStatusErr(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"Permission denied."}}`))
	if plain.CredentialNeedsAction() {
		t.Fatal("plain 403 must not request credential action")
	}

	// 429 carries a Retry-After and never a verification reason.
	throttled := newAntigravityStatusErr(http.StatusTooManyRequests, []byte(`{"error":{"code":429,"message":"Resource exhausted."}}`))
	if throttled.CredentialNeedsAction() {
		t.Fatal("429 must not request credential action")
	}
}
