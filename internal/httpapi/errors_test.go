package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestErrorCatalog(t *testing.T) {
	tests := []struct {
		name         string
		err          *Error
		status       int
		code         Code
		message      string
		authenticate string
		allow        string
		retryAfter   string
	}{
		{
			name:    "invalid request",
			err:     InvalidRequest(),
			status:  http.StatusBadRequest,
			code:    CodeInvalidRequest,
			message: "The supplied request is invalid.",
		},
		{
			name:    "invalid oauth state",
			err:     InvalidOAuthState(),
			status:  http.StatusBadRequest,
			code:    CodeInvalidOAuthState,
			message: "The OAuth state is invalid.",
		},
		{
			name:    "oauth authorization failed",
			err:     OAuthAuthorizationFailed(),
			status:  http.StatusBadRequest,
			code:    CodeOAuthAuthorizationFailed,
			message: "OAuth authorization failed.",
		},
		{
			name:         "unauthenticated",
			err:          Unauthenticated(),
			status:       http.StatusUnauthorized,
			code:         CodeUnauthenticated,
			message:      "A valid access token is required.",
			authenticate: "Bearer",
		},
		{
			name:         "invalid token",
			err:          InvalidToken(),
			status:       http.StatusUnauthorized,
			code:         CodeUnauthenticated,
			message:      "A valid access token is required.",
			authenticate: `Bearer error="invalid_token"`,
		},
		{
			name:    "not found",
			err:     NotFound(),
			status:  http.StatusNotFound,
			code:    CodeNotFound,
			message: "The requested resource was not found.",
		},
		{
			name:    "method not allowed",
			err:     MethodNotAllowed("GET, POST"),
			status:  http.StatusMethodNotAllowed,
			code:    CodeMethodNotAllowed,
			message: "The requested method is not allowed.",
			allow:   "GET, POST",
		},
		{
			name:    "request too large",
			err:     RequestTooLarge(),
			status:  http.StatusRequestEntityTooLarge,
			code:    CodeRequestTooLarge,
			message: "The request body is too large.",
		},
		{
			name:    "unsupported media type",
			err:     UnsupportedMediaType(),
			status:  http.StatusUnsupportedMediaType,
			code:    CodeUnsupportedMediaType,
			message: "The request content type is not supported.",
		},
		{
			name:       "rate limit",
			err:        RateLimited(24 * time.Second),
			status:     http.StatusTooManyRequests,
			code:       CodeRateLimitExceeded,
			message:    "The project has reached its sandbox creation request limit.",
			retryAfter: "24",
		},
		{
			name:    "quota",
			err:     QuotaExceeded(),
			status:  http.StatusTooManyRequests,
			code:    CodeQuotaExceeded,
			message: "The project has reached its running sandbox limit.",
		},
		{
			name:    "internal",
			err:     Internal(),
			status:  http.StatusInternalServerError,
			code:    CodeInternalError,
			message: "The request could not be completed.",
		},
		{
			name:    "oauth provider",
			err:     OAuthProviderError(),
			status:  http.StatusBadGateway,
			code:    CodeOAuthProviderError,
			message: "The OAuth provider returned an invalid response.",
		},
		{
			name:    "unavailable",
			err:     ServiceUnavailable(),
			status:  http.StatusServiceUnavailable,
			code:    CodeServiceUnavailable,
			message: "A required service is unavailable.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteError(rec, tt.err)
			assertAPIError(t, rec, tt.status, tt.code, tt.message)
			if rec.Header().Get("WWW-Authenticate") != tt.authenticate {
				t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
			}
			if rec.Header().Get("Allow") != tt.allow {
				t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
			}
			if rec.Header().Get("Retry-After") != tt.retryAfter {
				t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}
}

func TestRetryAfterRoundsUpToWholeSeconds(t *testing.T) {
	tests := []struct {
		remaining time.Duration
		want      string
	}{
		{remaining: 0, want: "1"},
		{remaining: -time.Second, want: "1"},
		{remaining: time.Nanosecond, want: "1"},
		{remaining: time.Millisecond, want: "1"},
		{remaining: 1000 * time.Millisecond, want: "1"},
		{remaining: 1001 * time.Millisecond, want: "2"},
		{remaining: 1500 * time.Millisecond, want: "2"},
		{remaining: 24 * time.Second, want: "24"},
		{remaining: 24200 * time.Millisecond, want: "25"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteError(rec, RateLimited(tt.remaining))
			if rec.Header().Get("Retry-After") != tt.want {
				t.Fatalf("Retry-After = %q, want %s", rec.Header().Get("Retry-After"), tt.want)
			}
		})
	}
}

func TestWriteErrorHidesInternalDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	secret := "pq: password=secret token=abc"
	WriteError(rec, fmt.Errorf("%s", secret))
	if strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), "pq:") {
		t.Fatalf("body leaked internal error: %s", rec.Body.String())
	}
	assertAPIError(t, rec, http.StatusInternalServerError, CodeInternalError, "The request could not be completed.")
}

func TestWriteErrorMapsMaxBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, &http.MaxBytesError{Limit: MaxRequestBodyBytes})
	assertAPIError(t, rec, http.StatusRequestEntityTooLarge, CodeRequestTooLarge, "The request body is too large.")
}

func TestQuotaHasNoRetryAfter(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, QuotaExceeded())
	if rec.Header().Get("Retry-After") != "" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if rec.Code == http.StatusForbidden {
		t.Fatal("status = 403")
	}
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code Code, message string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, status, rec.Body.String())
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	var body errorResponse
	if err := dec.Decode(&body); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing data: %v", err)
	}
	if body.Error.Code != string(code) || body.Error.Message != message {
		t.Fatalf("error = %#v", body.Error)
	}
}
