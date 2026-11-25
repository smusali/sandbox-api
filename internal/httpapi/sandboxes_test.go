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

func TestListSandboxes(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	newerID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	olderID := uuid.MustParse("7719ac21-1b89-4865-89b5-0ed2f8721003")
	newerAt := time.Date(2026, 11, 24, 12, 10, 0, 0, time.UTC)
	olderAt := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	stoppedAt := time.Date(2026, 11, 24, 12, 5, 0, 0, time.UTC)

	var gotUser uuid.UUID
	var gotProject uuid.UUID
	var gotPage sandboxes.Page
	handler := NewSandboxes(fakeSandboxStore{
		listFn: func(_ context.Context, user, project uuid.UUID, page sandboxes.Page) ([]db.Sandbox, error) {
			gotUser = user
			gotProject = project
			gotPage = page
			return []db.Sandbox{
				{ID: newerID, ProjectID: projectID, Status: SandboxRunning, CreatedAt: newerAt},
				{ID: olderID, ProjectID: projectID, Status: SandboxStopped, CreatedAt: olderAt, StoppedAt: &stoppedAt},
			}, nil
		},
	}, time.Second).List()

	req, rec := listSandboxRequest(projectID.String(), "limit=1", "")
	handler.ServeHTTP(rec, withUser(req, userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if gotUser != userID || gotProject != projectID {
		t.Fatalf("stored user %s project %s", gotUser, gotProject)
	}
	if gotPage.Limit != 2 || gotPage.After != nil {
		t.Fatalf("page = %+v", gotPage)
	}
	next, err := NextCursor(CollectionSandboxes, NewID(projectID), newerAt, NewID(newerID), true)
	if err != nil || next == nil {
		t.Fatalf("cursor: %v", err)
	}
	want := `{"items":[{"id":"44444444-4444-4444-8444-444444444444","project_id":"22222222-2222-4222-8222-222222222222","status":"running","created_at":"2026-11-24T12:10:00Z","stopped_at":null}],"next_cursor":"` + *next + `"}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s", rec.Body.String())
	}

	var followed sandboxes.Page
	follow := NewSandboxes(fakeSandboxStore{
		listFn: func(_ context.Context, _ uuid.UUID, _ uuid.UUID, page sandboxes.Page) ([]db.Sandbox, error) {
			followed = page
			return []db.Sandbox{{
				ID:        olderID,
				ProjectID: projectID,
				Status:    SandboxStopped,
				CreatedAt: olderAt,
				StoppedAt: &stoppedAt,
			}}, nil
		},
	}, time.Second).List()
	nextReq, nextRec := listSandboxRequest(projectID.String(), "limit=1&cursor="+*next, "")
	follow.ServeHTTP(nextRec, withUser(nextReq, userID))
	if nextRec.Code != http.StatusOK {
		t.Fatalf("next status = %d, body %s", nextRec.Code, nextRec.Body.String())
	}
	if followed.After == nil || followed.After.ID != newerID || !followed.After.CreatedAt.Equal(newerAt) || followed.Limit != 2 {
		t.Fatalf("followed page = %+v", followed)
	}
	const rest = `{"items":[{"id":"7719ac21-1b89-4865-89b5-0ed2f8721003","project_id":"22222222-2222-4222-8222-222222222222","status":"stopped","created_at":"2026-11-24T12:00:00Z","stopped_at":"2026-11-24T12:05:00Z"}],"next_cursor":null}`
	if nextRec.Body.String() != rest {
		t.Fatalf("next body = %s", nextRec.Body.String())
	}
}

func TestListSandboxesEmpty(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	span, err := Open(Page{Limit: DefaultPageLimit}, CollectionSandboxes, NewID(projectID))
	if err != nil {
		t.Fatalf("span: %v", err)
	}

	var gotPage sandboxes.Page
	handler := NewSandboxes(fakeSandboxStore{
		listFn: func(_ context.Context, user, project uuid.UUID, page sandboxes.Page) ([]db.Sandbox, error) {
			if user != userID || project != projectID {
				t.Fatalf("stored user %s project %s", user, project)
			}
			gotPage = page
			return []db.Sandbox{}, nil
		},
	}, time.Second).List()

	req, rec := listSandboxRequest(projectID.String(), "", "")
	handler.ServeHTTP(rec, withUser(req, userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if gotPage.Limit != span.Limit() || gotPage.After != nil {
		t.Fatalf("page = %+v", gotPage)
	}
	if rec.Body.String() != `{"items":[],"next_cursor":null}` {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestListSandboxesRejectsInvalidRequest(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := "22222222-2222-4222-8222-222222222222"
	otherProject := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	sandboxID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	created := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	foreign, err := NextCursor(CollectionSandboxes, NewID(otherProject), created, NewID(sandboxID), true)
	if err != nil || foreign == nil {
		t.Fatalf("sandbox cursor: %v", err)
	}
	projectCursor, err := NextCursor(CollectionProjects, NewID(userID), created, NewID(sandboxID), true)
	if err != nil || projectCursor == nil {
		t.Fatalf("project cursor: %v", err)
	}

	tests := []struct {
		name      string
		projectID string
		query     string
		body      string
	}{
		{name: "project", projectID: "not-a-uuid"},
		{name: "limit", projectID: projectID, query: "limit=0"},
		{name: "cursor", projectID: projectID, query: "cursor=%25%25%25"},
		{name: "other project", projectID: projectID, query: "cursor=" + *foreign},
		{name: "project cursor", projectID: projectID, query: "cursor=" + *projectCursor},
		{name: "body", projectID: projectID, body: `{"status":"running"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewSandboxes(rejectSandboxStore(t), time.Second).List()
			req, rec := listSandboxRequest(tt.projectID, tt.query, tt.body)
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, http.StatusBadRequest, CodeInvalidRequest, "The supplied request is invalid.")
		})
	}
}

func TestListSandboxesMapsErrors(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := "22222222-2222-4222-8222-222222222222"
	tests := []struct {
		name    string
		err     error
		status  int
		code    Code
		message string
	}{
		{
			name:    "not found",
			err:     sandboxes.ErrNotFound,
			status:  http.StatusNotFound,
			code:    CodeNotFound,
			message: "The requested resource was not found.",
		},
		{
			name:    "unavailable",
			err:     context.DeadlineExceeded,
			status:  http.StatusServiceUnavailable,
			code:    CodeServiceUnavailable,
			message: "A required service is unavailable.",
		},
		{
			name:    "failed read",
			err:     errors.New("select failed"),
			status:  http.StatusInternalServerError,
			code:    CodeInternalError,
			message: "The request could not be completed.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewSandboxes(fakeSandboxStore{
				listFn: func(context.Context, uuid.UUID, uuid.UUID, sandboxes.Page) ([]db.Sandbox, error) {
					return nil, tt.err
				},
			}, time.Second).List()
			req, rec := listSandboxRequest(projectID, "", "")
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, tt.status, tt.code, tt.message)
		})
	}
}

func TestStopSandbox(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	sandboxID := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	var gotUser uuid.UUID
	var gotSandbox uuid.UUID
	handler := NewSandboxes(fakeSandboxStore{
		stopFn: func(_ context.Context, user, sandbox uuid.UUID) error {
			gotUser = user
			gotSandbox = sandbox
			return nil
		},
	}, time.Second).Stop()

	req, rec := stopSandboxRequest(sandboxID.String(), "")
	handler.ServeHTTP(rec, withUser(req, userID))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if gotUser != userID || gotSandbox != sandboxID {
		t.Fatalf("stored user %s sandbox %s", gotUser, gotSandbox)
	}
}

func TestStopSandboxRejectsInvalidRequest(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	sandboxID := "44444444-4444-4444-8444-444444444444"
	tests := []struct {
		name      string
		sandboxID string
		body      string
	}{
		{name: "sandbox uuid", sandboxID: "not-a-uuid"},
		{name: "body", sandboxID: sandboxID, body: `{"status":"stopped"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewSandboxes(rejectSandboxStore(t), time.Second).Stop()
			req, rec := stopSandboxRequest(tt.sandboxID, tt.body)
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, http.StatusBadRequest, CodeInvalidRequest, "The supplied request is invalid.")
		})
	}
}

func TestStopSandboxMapsErrors(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	sandboxID := "44444444-4444-4444-8444-444444444444"
	tests := []struct {
		name    string
		err     error
		status  int
		code    Code
		message string
	}{
		{
			name:    "not found",
			err:     sandboxes.ErrNotFound,
			status:  http.StatusNotFound,
			code:    CodeNotFound,
			message: "The requested resource was not found.",
		},
		{
			name:    "unavailable",
			err:     context.DeadlineExceeded,
			status:  http.StatusServiceUnavailable,
			code:    CodeServiceUnavailable,
			message: "A required service is unavailable.",
		},
		{
			name:    "failed update",
			err:     errors.New("update failed"),
			status:  http.StatusInternalServerError,
			code:    CodeInternalError,
			message: "The request could not be completed.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewSandboxes(fakeSandboxStore{
				stopFn: func(context.Context, uuid.UUID, uuid.UUID) error {
					return tt.err
				},
			}, time.Second).Stop()
			req, rec := stopSandboxRequest(sandboxID, "")
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, tt.status, tt.code, tt.message)
		})
	}
}

func TestStopSandboxRequiresPrincipal(t *testing.T) {
	handler := NewSandboxes(rejectSandboxStore(t), time.Second).Stop()
	req, rec := stopSandboxRequest("44444444-4444-4444-8444-444444444444", "")
	handler.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusInternalServerError, CodeInternalError, "The request could not be completed.")
}

func TestListSandboxesRequiresPrincipal(t *testing.T) {
	handler := NewSandboxes(rejectSandboxStore(t), time.Second).List()
	req, rec := listSandboxRequest("22222222-2222-4222-8222-222222222222", "", "")
	handler.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusInternalServerError, CodeInternalError, "The request could not be completed.")
}

func stopSandboxRequest(sandboxID, body string) (*http.Request, *httptest.ResponseRecorder) {
	req, rec := bodyRequest(http.MethodDelete, "/v1/sandboxes/"+sandboxID, "", body)
	req.SetPathValue("sandboxID", sandboxID)
	return req, rec
}

func sandboxRequest(projectID, contentType, body string) (*http.Request, *httptest.ResponseRecorder) {
	req, rec := bodyRequest(http.MethodPost, "/v1/projects/"+projectID+"/sandboxes", contentType, body)
	req.SetPathValue("projectID", projectID)
	return req, rec
}

func listSandboxRequest(projectID, rawQuery, body string) (*http.Request, *httptest.ResponseRecorder) {
	target := "/v1/projects/" + projectID + "/sandboxes"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, rec := bodyRequest(http.MethodGet, target, "", body)
	req.SetPathValue("projectID", projectID)
	return req, rec
}

type fakeSandboxStore struct {
	createFn func(context.Context, uuid.UUID, uuid.UUID) (db.Sandbox, error)
	listFn   func(context.Context, uuid.UUID, uuid.UUID, sandboxes.Page) ([]db.Sandbox, error)
	stopFn   func(context.Context, uuid.UUID, uuid.UUID) error
}

func (f fakeSandboxStore) Create(ctx context.Context, userID, projectID uuid.UUID) (db.Sandbox, error) {
	return f.createFn(ctx, userID, projectID)
}

func (f fakeSandboxStore) List(ctx context.Context, userID, projectID uuid.UUID, page sandboxes.Page) ([]db.Sandbox, error) {
	return f.listFn(ctx, userID, projectID, page)
}

func (f fakeSandboxStore) Stop(ctx context.Context, userID, sandboxID uuid.UUID) error {
	return f.stopFn(ctx, userID, sandboxID)
}

func rejectSandboxStore(t *testing.T) fakeSandboxStore {
	t.Helper()
	return fakeSandboxStore{
		createFn: func(context.Context, uuid.UUID, uuid.UUID) (db.Sandbox, error) {
			t.Helper()
			t.Fatal("create called")
			return db.Sandbox{}, nil
		},
		listFn: func(context.Context, uuid.UUID, uuid.UUID, sandboxes.Page) ([]db.Sandbox, error) {
			t.Helper()
			t.Fatal("list called")
			return nil, nil
		},
		stopFn: func(context.Context, uuid.UUID, uuid.UUID) error {
			t.Helper()
			t.Fatal("stop called")
			return nil
		},
	}
}
