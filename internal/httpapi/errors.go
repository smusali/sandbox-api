// Package httpapi provides the shared HTTP contracts for the API.
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Code is a stable machine-readable error identifier.
type Code string

const (
	CodeInvalidRequest           Code = "INVALID_REQUEST"
	CodeInvalidOAuthState        Code = "INVALID_OAUTH_STATE"
	CodeOAuthAuthorizationFailed Code = "OAUTH_AUTHORIZATION_FAILED"
	CodeUnauthenticated          Code = "UNAUTHENTICATED"
	CodeNotFound                 Code = "NOT_FOUND"
	CodeMethodNotAllowed         Code = "METHOD_NOT_ALLOWED"
	CodeRequestTooLarge          Code = "REQUEST_TOO_LARGE"
	CodeUnsupportedMediaType     Code = "UNSUPPORTED_MEDIA_TYPE"
	CodeRateLimitExceeded        Code = "RATE_LIMIT_EXCEEDED"
	CodeQuotaExceeded            Code = "QUOTA_EXCEEDED"
	CodeInternalError            Code = "INTERNAL_ERROR"
	CodeOAuthProviderError       Code = "OAUTH_PROVIDER_ERROR"
	CodeServiceUnavailable       Code = "SERVICE_UNAVAILABLE"
)

const (
	msgInvalidRequest           = "The supplied request is invalid."
	msgInvalidOAuthState        = "The OAuth state is invalid."
	msgOAuthAuthorizationFailed = "OAuth authorization failed."
	msgUnauthenticated          = "A valid access token is required."
	msgNotFound                 = "The requested resource was not found."
	msgMethodNotAllowed         = "The requested method is not allowed."
	msgRequestTooLarge          = "The request body is too large."
	msgUnsupportedMediaType     = "The request content type is not supported."
	msgRateLimitExceeded        = "The project has reached its sandbox creation request limit."
	msgQuotaExceeded            = "The project has reached its running sandbox limit."
	msgInternalError            = "The request could not be completed."
	msgOAuthProviderError       = "The OAuth provider returned an invalid response."
	msgServiceUnavailable       = "A required service is unavailable."

	challengeBearer       = "Bearer"
	challengeInvalidToken = `Bearer error="invalid_token"`
)

// Error is one public API failure.
type Error struct {
	Status          int
	Code            Code
	Message         string
	WWWAuthenticate string
	Allow           string
	RetryAfter      string
}

func (e *Error) Error() string {
	if e == nil {
		return msgInternalError
	}
	return e.Message
}

// InvalidRequest reports a malformed or rejected request.
func InvalidRequest() *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: msgInvalidRequest}
}

// InvalidOAuthState reports an OAuth state that cannot be accepted.
func InvalidOAuthState() *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeInvalidOAuthState, Message: msgInvalidOAuthState}
}

// OAuthAuthorizationFailed reports a rejected authorization-code exchange.
func OAuthAuthorizationFailed() *Error {
	return &Error{Status: http.StatusBadRequest, Code: CodeOAuthAuthorizationFailed, Message: msgOAuthAuthorizationFailed}
}

// Unauthenticated reports a request that did not present usable credentials.
func Unauthenticated() *Error {
	return &Error{
		Status:          http.StatusUnauthorized,
		Code:            CodeUnauthenticated,
		Message:         msgUnauthenticated,
		WWWAuthenticate: challengeBearer,
	}
}

// InvalidToken reports a presented credential that failed verification.
func InvalidToken() *Error {
	return &Error{
		Status:          http.StatusUnauthorized,
		Code:            CodeUnauthenticated,
		Message:         msgUnauthenticated,
		WWWAuthenticate: challengeInvalidToken,
	}
}

// NotFound reports a missing route or a missing or inaccessible resource.
func NotFound() *Error {
	return &Error{Status: http.StatusNotFound, Code: CodeNotFound, Message: msgNotFound}
}

// MethodNotAllowed reports a method the route does not serve.
// allow is the Allow header value.
func MethodNotAllowed(allow string) *Error {
	return &Error{
		Status:  http.StatusMethodNotAllowed,
		Code:    CodeMethodNotAllowed,
		Message: msgMethodNotAllowed,
		Allow:   allow,
	}
}

// RequestTooLarge reports a body above the accepted size.
func RequestTooLarge() *Error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Code: CodeRequestTooLarge, Message: msgRequestTooLarge}
}

// UnsupportedMediaType reports a body whose content type is not JSON.
func UnsupportedMediaType() *Error {
	return &Error{Status: http.StatusUnsupportedMediaType, Code: CodeUnsupportedMediaType, Message: msgUnsupportedMediaType}
}

// RateLimited reports a full creation window.
// remaining is the time left in that window.
func RateLimited(remaining time.Duration) *Error {
	return &Error{
		Status:     http.StatusTooManyRequests,
		Code:       CodeRateLimitExceeded,
		Message:    msgRateLimitExceeded,
		RetryAfter: strconv.Itoa(retryAfterSeconds(remaining)),
	}
}

// QuotaExceeded reports a project that is already at its running sandbox limit.
func QuotaExceeded() *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: CodeQuotaExceeded, Message: msgQuotaExceeded}
}

// Internal reports an unexpected application failure.
func Internal() *Error {
	return &Error{Status: http.StatusInternalServerError, Code: CodeInternalError, Message: msgInternalError}
}

// OAuthProviderError reports a malformed or unexpected upstream OAuth response.
func OAuthProviderError() *Error {
	return &Error{Status: http.StatusBadGateway, Code: CodeOAuthProviderError, Message: msgOAuthProviderError}
}

// ServiceUnavailable reports a required dependency that cannot be reached.
func ServiceUnavailable() *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: CodeServiceUnavailable, Message: msgServiceUnavailable}
}

// WriteError writes err using the public error contract.
// Errors that are not part of that contract become an internal error.
func WriteError(w http.ResponseWriter, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		writeAPIError(w, RequestTooLarge())
		return
	}
	if err != nil {
		log.Printf("internal error: %v", err)
	}
	writeAPIError(w, Internal())
}

func writeAPIError(w http.ResponseWriter, apiErr *Error) {
	if apiErr.WWWAuthenticate != "" {
		w.Header().Set("WWW-Authenticate", apiErr.WWWAuthenticate)
	}
	if apiErr.Allow != "" {
		w.Header().Set("Allow", apiErr.Allow)
	}
	if apiErr.RetryAfter != "" {
		w.Header().Set("Retry-After", apiErr.RetryAfter)
	}
	writeBody(w, apiErr.Status, encodeError(apiErr))
}

func encodeError(apiErr *Error) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	body := errorResponse{Error: errorObject{Code: string(apiErr.Code), Message: apiErr.Message}}
	if err := enc.Encode(body); err != nil {
		return internalErrorJSON
	}
	return trimNewline(buf.Bytes())
}

type errorResponse struct {
	Error errorObject `json:"error"`
}

type errorObject struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func retryAfterSeconds(remaining time.Duration) int {
	if remaining <= 0 {
		return 1
	}
	milliseconds := (remaining + time.Millisecond - 1) / time.Millisecond
	seconds := (milliseconds + 999) / 1000
	if seconds < 1 {
		return 1
	}
	return int(seconds)
}
