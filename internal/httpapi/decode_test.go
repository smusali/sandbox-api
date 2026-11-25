package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeCreateProject(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		wantName    string
		wantCode    Code
	}{
		{name: "trimmed name", body: `{"name":"  Agent Development  "}`, contentType: "application/json", wantName: "Agent Development"},
		{name: "charset", body: `{"name":"Agent"}`, contentType: "Application/JSON; charset=UTF-8", wantName: "Agent"},
		{name: "trailing space", body: `{"name":"Agent"}   `, contentType: "application/json", wantName: "Agent"},
		{name: "missing content type", body: `{"name":"Agent"}`, wantCode: CodeUnsupportedMediaType},
		{name: "text body", body: `{"name":"Agent"}`, contentType: "text/plain", wantCode: CodeUnsupportedMediaType},
		{name: "utf-16", body: `{"name":"Agent"}`, contentType: "application/json; charset=utf-16", wantCode: CodeUnsupportedMediaType},
		{name: "extra parameter", body: `{"name":"Agent"}`, contentType: "application/json; charset=utf-8; foo=bar", wantCode: CodeUnsupportedMediaType},
		{name: "empty", body: ``, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "empty object", body: `{}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "blank name", body: `{"name":"   "}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "null object", body: `null`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "array", body: `[]`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "number", body: `1`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "name type", body: `{"name":1}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "name null", body: `{"name":null}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "unknown id", body: `{"name":"Agent","id":"6d502949-7e8c-4f47-8b0c-3f1e3b913001"}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "unknown quota", body: `{"name":"Agent","max_running_sandboxes":10}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "unknown creator", body: `{"name":"Agent","user_id":"de498973-c030-4fc2-b075-4b5dbb421002"}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "trailing value", body: `{"name":"Agent"}{"name":"Other"}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "malformed", body: `{"name":`, contentType: "application/json", wantCode: CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, rec := bodyRequest(http.MethodPost, "/v1/projects", tt.contentType, tt.body)
			got, err := DecodeCreateProject(rec, req)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				if got.Name != tt.wantName {
					t.Fatalf("name = %q", got.Name)
				}
				return
			}
			assertDecodeError(t, err, tt.wantCode)
		})
	}
}

func TestDecodeCreateProjectBodyLimit(t *testing.T) {
	exact := "{}" + strings.Repeat(" ", MaxRequestBodyBytes-2)
	req, rec := bodyRequest(http.MethodPost, "/v1/projects", "application/json", exact)
	_, err := DecodeCreateProject(rec, req)
	assertDecodeError(t, err, CodeInvalidRequest)

	over := "{}" + strings.Repeat(" ", MaxRequestBodyBytes-1)
	req, rec = bodyRequest(http.MethodPost, "/v1/projects", "application/json", over)
	_, err = DecodeCreateProject(rec, req)
	assertDecodeError(t, err, CodeRequestTooLarge)
}

func TestDecodeAddMember(t *testing.T) {
	valid := "de498973-c030-4fc2-b075-4b5dbb421002"
	tests := []struct {
		name     string
		body     string
		wantID   string
		wantCode Code
	}{
		{name: "member", body: `{"user_id":"` + valid + `"}`, wantID: valid},
		{name: "uppercase", body: `{"user_id":"DE498973-C030-4FC2-B075-4B5DBB421002"}`, wantID: valid},
		{name: "nil uuid", body: `{"user_id":"00000000-0000-0000-0000-000000000000"}`, wantID: "00000000-0000-0000-0000-000000000000"},
		{name: "missing", body: `{}`, wantCode: CodeInvalidRequest},
		{name: "null", body: `{"user_id":null}`, wantCode: CodeInvalidRequest},
		{name: "number", body: `{"user_id":1}`, wantCode: CodeInvalidRequest},
		{name: "braces", body: `{"user_id":"{de498973-c030-4fc2-b075-4b5dbb421002}"}`, wantCode: CodeInvalidRequest},
		{name: "urn", body: `{"user_id":"urn:uuid:de498973-c030-4fc2-b075-4b5dbb421002"}`, wantCode: CodeInvalidRequest},
		{name: "bare hex", body: `{"user_id":"de498973c0304fc2b0754b5dbb421002"}`, wantCode: CodeInvalidRequest},
		{name: "unknown field", body: `{"user_id":"` + valid + `","project_id":"` + valid + `"}`, wantCode: CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, rec := bodyRequest(http.MethodPost, "/v1/projects/"+valid+"/members", "application/json", tt.body)
			got, err := DecodeAddMember(rec, req)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				if got.UserID == nil || got.UserID.String() != tt.wantID {
					t.Fatalf("user id = %#v", got.UserID)
				}
				return
			}
			assertDecodeError(t, err, tt.wantCode)
		})
	}
}

func TestDecodeCreateSandbox(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		contentType string
		wantCode    Code
	}{
		{name: "empty", body: ""},
		{name: "empty object", body: `{}`, contentType: "application/json"},
		{name: "spaced object", body: " { } ", contentType: "application/json"},
		{name: "empty without type but typed header ignored", body: "", contentType: "text/plain"},
		{name: "object without type", body: `{}`, wantCode: CodeUnsupportedMediaType},
		{name: "whitespace", body: " \n", contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "status", body: `{"status":"running"}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "id", body: `{"id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "null", body: `null`, contentType: "application/json", wantCode: CodeInvalidRequest},
		{name: "array", body: `[]`, contentType: "application/json", wantCode: CodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, rec := bodyRequest(http.MethodPost, "/v1/projects/x/sandboxes", tt.contentType, tt.body)
			err := DecodeCreateSandbox(rec, req)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				return
			}
			assertDecodeError(t, err, tt.wantCode)
		})
	}
}

func TestRejectBody(t *testing.T) {
	emptyReq, emptyRec := bodyRequest(http.MethodGet, "/v1/projects", "", "")
	if err := RejectBody(emptyRec, emptyReq); err != nil {
		t.Fatalf("empty error = %v", err)
	}

	smallReq, smallRec := bodyRequest(http.MethodDelete, "/v1/sandboxes/x", "application/json", `{}`)
	assertDecodeError(t, RejectBody(smallRec, smallReq), CodeInvalidRequest)

	over := strings.Repeat("x", MaxRequestBodyBytes+1)
	largeReq, largeRec := bodyRequest(http.MethodGet, "/v1/projects", "text/plain", over)
	assertDecodeError(t, RejectBody(largeRec, largeReq), CodeRequestTooLarge)
}

func bodyRequest(method, target, contentType, body string) (*http.Request, *httptest.ResponseRecorder) {
	var reader *strings.Reader
	if body != "" || contentType != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, reader)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, httptest.NewRecorder()
}

func assertDecodeError(t *testing.T, err error, code Code) {
	t.Helper()
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr == nil {
		t.Fatalf("error = %v", err)
	}
	if apiErr.Code != code {
		t.Fatalf("code = %s, want %s", apiErr.Code, code)
	}
}
