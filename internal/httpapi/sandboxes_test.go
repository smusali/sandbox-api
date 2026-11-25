package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/ratelimit"
	"github.com/smusali/sandbox-api/internal/sandboxes"
)

func TestCreateSandbox(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	sandboxID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	created := time.Date(2026, 11, 24, 12, 10, 0, 0, time.UTC)

	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "empty body"},
		{name: "empty object", contentType: "application/json", body: "{}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotUser uuid.UUID
			var gotProject uuid.UUID
			handler := NewSandboxes(fakeSandboxStore{
				createFn: func(_ context.Context, user uuid.UUID, project uuid.UUID) (db.Sandbox, error) {
					gotUser = user
					gotProject = project
					return db.Sandbox{
						ID:        sandboxID,
						ProjectID: projectID,
						Status:    SandboxRunning,
						CreatedAt: created,
					}, nil
				},
			}, time.Second).Create()

			req, rec := sandboxRequest(projectID.String(), tt.contentType, tt.body)
			handler.ServeHTTP(rec, withUser(req, userID))

			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
			const want = `{"id":"44444444-4444-4444-8444-444444444444","project_id":"22222222-2222-4222-8222-222222222222","status":"running","created_at":"2026-11-24T12:10:00Z","stopped_at":null}`
			if rec.Body.String() != want {
				t.Fatalf("body = %s", rec.Body.String())
			}
			if gotUser != userID || gotProject != projectID {
				t.Fatalf("stored user %s project %s", gotUser, gotProject)
			}
		})
	}
}

func TestCreateSandboxRejectsInvalidRequest(t *testing.T) {
	projectID := "22222222-2222-4222-8222-222222222222"
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	tests := []struct {
		name        string
		projectID   string
		contentType string
		body        string
	}{
		{name: "id", projectID: projectID, contentType: "application/json", body: `{"id":"44444444-4444-4444-8444-444444444444"}`},
		{name: "status", projectID: projectID, contentType: "application/json", body: `{"status":"running"}`},
		{name: "project", projectID: projectID, contentType: "application/json", body: `{"project_id":"22222222-2222-4222-8222-222222222222"}`},
		{name: "owner", projectID: projectID, contentType: "application/json", body: `{"user_id":"de498973-c030-4fc2-b075-4b5dbb421002"}`},
		{name: "project uuid", projectID: "not-a-uuid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewSandboxes(rejectSandboxStore(t), time.Second).Create()
			req, rec := sandboxRequest(tt.projectID, tt.contentType, tt.body)
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, http.StatusBadRequest, CodeInvalidRequest, "The supplied request is invalid.")
		})
	}
}

func TestCreateSandboxMapsErrors(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := "22222222-2222-4222-8222-222222222222"
	tests := []struct {
		name       string
		err        error
		status     int
		code       Code
		message    string
		retryAfter string
	}{
		{
			name:    "not found",
			err:     sandboxes.ErrNotFound,
			status:  http.StatusNotFound,
			code:    CodeNotFound,
			message: "The requested resource was not found.",
		},
		{
			name:       "rate limit",
			err:        &sandboxes.RateLimitedError{Remaining: 24 * time.Second},
			status:     http.StatusTooManyRequests,
			code:       CodeRateLimitExceeded,
			message:    "The project has reached its sandbox creation request limit.",
			retryAfter: "24",
		},
		{
			name:    "quota",
			err:     sandboxes.ErrQuotaExceeded,
			status:  http.StatusTooManyRequests,
			code:    CodeQuotaExceeded,
			message: "The project has reached its running sandbox limit.",
		},
		{
			name:    "admission unavailable",
			err:     ratelimit.ErrUnavailable,
			status:  http.StatusServiceUnavailable,
			code:    CodeServiceUnavailable,
			message: "A required service is unavailable.",
		},
		{
			name:    "database unavailable",
			err:     context.DeadlineExceeded,
			status:  http.StatusServiceUnavailable,
			code:    CodeServiceUnavailable,
			message: "A required service is unavailable.",
		},
		{
			name:    "failed insert",
			err:     errors.New("insert failed"),
			status:  http.StatusInternalServerError,
			code:    CodeInternalError,
			message: "The request could not be completed.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewSandboxes(fakeSandboxStore{
				createFn: func(context.Context, uuid.UUID, uuid.UUID) (db.Sandbox, error) {
					return db.Sandbox{}, tt.err
				},
			}, time.Second).Create()
			req, rec := sandboxRequest(projectID, "application/json", "{}")
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, tt.status, tt.code, tt.message)
			if rec.Header().Get("Retry-After") != tt.retryAfter {
				t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
			}
		})
	}
}

func sandboxRequest(projectID, contentType, body string) (*http.Request, *httptest.ResponseRecorder) {
	req, rec := bodyRequest(http.MethodPost, "/v1/projects/"+projectID+"/sandboxes", contentType, body)
	req.SetPathValue("projectID", projectID)
	return req, rec
}

type fakeSandboxStore struct {
	createFn func(context.Context, uuid.UUID, uuid.UUID) (db.Sandbox, error)
}

func (f fakeSandboxStore) Create(ctx context.Context, userID, projectID uuid.UUID) (db.Sandbox, error) {
	return f.createFn(ctx, userID, projectID)
}

func rejectSandboxStore(t *testing.T) fakeSandboxStore {
	t.Helper()
	return fakeSandboxStore{
		createFn: func(context.Context, uuid.UUID, uuid.UUID) (db.Sandbox, error) {
			t.Helper()
			t.Fatal("create called")
			return db.Sandbox{}, nil
		},
	}
}
