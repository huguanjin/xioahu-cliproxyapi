package executor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAntigravityRefreshTokenIsDead(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{
			// Google's answer for a refresh token the user revoked, or that was
			// already exchanged and rotated away.
			name: "invalid_grant",
			body: `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`,
			want: true,
		},
		{
			name: "invalid_grant uppercased",
			body: `{"error":"INVALID_GRANT"}`,
			want: true,
		},
		{
			name: "invalid_grant with surrounding whitespace",
			body: `{"error":"  invalid_grant  "}`,
			want: true,
		},
		{
			name: "refresh_token_revoked",
			body: `{"error":"refresh_token_revoked"}`,
			want: true,
		},
		{
			name: "refresh_token_reused",
			body: `{"error":"refresh_token_reused"}`,
			want: true,
		},
		{name: "empty body", body: "", want: false},
		{name: "non json body", body: "invalid_grant", want: false},
		{
			// A malformed request is our bug, not a dead credential.
			name: "generic invalid_request",
			body: `{"error":"invalid_request","error_description":"Missing required parameter: refresh_token"}`,
			want: false,
		},
		{
			// The app credentials are wrong; every refresh token would fail, so
			// disabling individual accounts would be wrong.
			name: "invalid_client",
			body: `{"error":"invalid_client"}`,
			want: false,
		},
		{
			// Transient upstream failure with no OAuth error code.
			name: "json without error field",
			body: `{"error_description":"Service unavailable"}`,
			want: false,
		},
		{
			// An access-token error must not be read as a refresh-token verdict.
			name: "unauthorized_client",
			body: `{"error":"unauthorized_client"}`,
			want: false,
		},
		{
			// The literal must be the whole error code, not a substring of one.
			name: "invalid_grant_x suffix",
			body: `{"error":"invalid_grant_x"}`,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityRefreshTokenIsDead([]byte(tc.body)); got != tc.want {
				t.Fatalf("antigravityRefreshTokenIsDead() = %v, want %v", got, tc.want)
			}
		})
	}
}

// refreshRejectedAuth builds an auth carrying a refresh token, the shape the
// dead-token detector inspects.
func refreshRejectedAuth(id, refreshToken string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID: id,
		Metadata: map[string]any{
			"refresh_token": refreshToken,
			"expired":       "2000-01-01T00:00:00Z",
		},
	}
}

func TestRefreshToken_FlagsRevokedRefreshToken(t *testing.T) {
	exec := NewAntigravityExecutor(&config.Config{})
	auth := refreshRejectedAuth("auth-revoked", "revoked-refresh-token")
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "oauth2.googleapis.com" {
			t.Fatalf("unexpected refresh host %s", req.URL.Host)
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)),
		}, nil
	}))

	_, err := exec.refreshToken(ctx, auth)
	if err == nil {
		t.Fatal("expected the refresh to fail")
	}
	flagged, ok := err.(interface{ CredentialNeedsAction() bool })
	if !ok {
		t.Fatalf("refresh error %T must expose CredentialNeedsAction", err)
	}
	if !flagged.CredentialNeedsAction() {
		t.Fatal("a revoked refresh token must request credential action")
	}
}

func TestRefreshToken_GenericFailureStaysTransient(t *testing.T) {
	exec := NewAntigravityExecutor(&config.Config{})
	auth := refreshRejectedAuth("auth-flaky", "flaky-refresh-token")
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_request","error_description":"Malformed request."}`)),
		}, nil
	}))

	_, err := exec.refreshToken(ctx, auth)
	if err == nil {
		t.Fatal("expected the refresh to fail")
	}
	if flagged, ok := err.(interface{ CredentialNeedsAction() bool }); ok && flagged.CredentialNeedsAction() {
		t.Fatal("a generic 400 must not request credential action")
	}
}
