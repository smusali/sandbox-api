package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/projects"
)

func TestCreateProject(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	created := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	var gotUser uuid.UUID
	var gotName string
	handler := NewProjects(fakeProjectStore{
		createFn: func(_ context.Context, id uuid.UUID, name string) (db.Project, error) {
			gotUser = id
			gotName = name
			return db.Project{
				ID:                  projectID,
				Name:                name,
				MaxRunningSandboxes: 10,
				CreationRateLimit:   20,
				CreatedAt:           created,
			}, nil
		},
	}, time.Second).Create()

	req, rec := bodyRequest(http.MethodPost, "/v1/projects", "application/json", `{"name":"  Agent Development  "}`)
	handler.ServeHTTP(rec, withUser(req, userID))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Cache-Control") != "" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	const want = `{"id":"22222222-2222-4222-8222-222222222222","name":"Agent Development","created_at":"2026-11-24T12:00:00Z"}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if gotUser != userID || gotName != "Agent Development" {
		t.Fatalf("stored user %s name %q", gotUser, gotName)
	}
}

func TestCreateProjectRejectsInvalidBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "blank name", body: `{"name":"   "}`},
		{name: "quota", body: `{"name":"Agent","max_running_sandboxes":10}`},
		{name: "creator", body: `{"name":"Agent","user_id":"de498973-c030-4fc2-b075-4b5dbb421002"}`},
	}
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewProjects(rejectProjectStore(t), time.Second).Create()
			req, rec := bodyRequest(http.MethodPost, "/v1/projects", "application/json", tt.body)
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, http.StatusBadRequest, CodeInvalidRequest, "The supplied request is invalid.")
		})
	}
}

func TestCreateProjectMapsPersistenceErrors(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	tests := []struct {
		name    string
		err     error
		status  int
		code    Code
		message string
	}{
		{
			name:    "unavailable",
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
			handler := NewProjects(fakeProjectStore{
				createFn: func(context.Context, uuid.UUID, string) (db.Project, error) {
					return db.Project{}, tt.err
				},
			}, time.Second).Create()
			req, rec := bodyRequest(http.MethodPost, "/v1/projects", "application/json", `{"name":"Agent"}`)
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, tt.status, tt.code, tt.message)
		})
	}
}

func TestListProjects(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	newerID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	olderID := uuid.MustParse("6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	newerAt := time.Date(2026, 11, 24, 12, 1, 0, 0, time.UTC)
	olderAt := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	var gotPage projects.Page
	handler := NewProjects(fakeProjectStore{
		listFn: func(_ context.Context, id uuid.UUID, page projects.Page) ([]db.Project, error) {
			if id != userID {
				t.Fatalf("user = %s", id)
			}
			gotPage = page
			return []db.Project{
				{ID: newerID, Name: "Newer", CreatedAt: newerAt},
				{ID: olderID, Name: "Older", CreatedAt: olderAt},
			}, nil
		},
	}, time.Second).List()

	req, rec := bodyRequest(http.MethodGet, "/v1/projects?limit=1", "", "")
	handler.ServeHTTP(rec, withUser(req, userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if gotPage.Limit != 2 || gotPage.After != nil {
		t.Fatalf("page = %+v", gotPage)
	}
	next, err := NextCursor(CollectionProjects, NewID(userID), newerAt, NewID(newerID), true)
	if err != nil || next == nil {
		t.Fatalf("cursor: %v", err)
	}
	want := `{"items":[{"id":"22222222-2222-4222-8222-222222222222","name":"Newer","created_at":"2026-11-24T12:01:00Z"}],"next_cursor":"` + *next + `"}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s", rec.Body.String())
	}

	var followed projects.Page
	follow := NewProjects(fakeProjectStore{
		listFn: func(_ context.Context, _ uuid.UUID, page projects.Page) ([]db.Project, error) {
			followed = page
			return []db.Project{{ID: olderID, Name: "Older", CreatedAt: olderAt}}, nil
		},
	}, time.Second).List()
	nextReq, nextRec := bodyRequest(http.MethodGet, "/v1/projects?limit=1&cursor="+*next, "", "")
	follow.ServeHTTP(nextRec, withUser(nextReq, userID))
	if nextRec.Code != http.StatusOK {
		t.Fatalf("next status = %d, body %s", nextRec.Code, nextRec.Body.String())
	}
	if followed.After == nil || followed.After.ID != newerID || !followed.After.CreatedAt.Equal(newerAt) || followed.Limit != 2 {
		t.Fatalf("followed page = %+v", followed)
	}
	const rest = `{"items":[{"id":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","name":"Older","created_at":"2026-11-24T12:00:00Z"}],"next_cursor":null}`
	if nextRec.Body.String() != rest {
		t.Fatalf("next body = %s", nextRec.Body.String())
	}
}

func TestListProjectsEmpty(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	handler := NewProjects(fakeProjectStore{
		listFn: func(context.Context, uuid.UUID, projects.Page) ([]db.Project, error) {
			return []db.Project{}, nil
		},
	}, time.Second).List()

	req, rec := bodyRequest(http.MethodGet, "/v1/projects", "", "")
	handler.ServeHTTP(rec, withUser(req, userID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != `{"items":[],"next_cursor":null}` {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestListProjectsRejectsInvalidQuery(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	otherID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	created := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	foreign, err := NextCursor(CollectionProjects, NewID(otherID), created, NewID(userID), true)
	if err != nil || foreign == nil {
		t.Fatalf("cursor: %v", err)
	}

	tests := []struct {
		name   string
		target string
		body   string
	}{
		{name: "limit", target: "/v1/projects?limit=0"},
		{name: "cursor", target: "/v1/projects?cursor=%25%25%25"},
		{name: "other user", target: "/v1/projects?cursor=" + *foreign},
		{name: "body", target: "/v1/projects", body: `{"name":"Agent"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewProjects(rejectProjectStore(t), time.Second).List()
			req, rec := bodyRequest(http.MethodGet, tt.target, "", tt.body)
			handler.ServeHTTP(rec, withUser(req, userID))
			assertAPIError(t, rec, http.StatusBadRequest, CodeInvalidRequest, "The supplied request is invalid.")
		})
	}
}

func TestAddMember(t *testing.T) {
	actorID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	targetID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	created := time.Date(2026, 11, 24, 12, 5, 0, 0, time.UTC)

	var gotActor, gotProject, gotTarget uuid.UUID
	handler := NewProjects(fakeProjectStore{
		addFn: func(_ context.Context, actor, project, target uuid.UUID) (projects.Member, error) {
			gotActor, gotProject, gotTarget = actor, project, target
			return projects.Member{
				ProjectID: project,
				UserID:    target,
				CreatedAt: created,
				Created:   true,
			}, nil
		},
	}, time.Second).AddMember()

	req, rec := memberRequest(projectID.String(), `{"user_id":"`+targetID.String()+`"}`)
	handler.ServeHTTP(rec, withUser(req, actorID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	const want = `{"project_id":"22222222-2222-4222-8222-222222222222","user_id":"33333333-3333-4333-8333-333333333333","created_at":"2026-11-24T12:05:00Z"}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if gotActor != actorID || gotProject != projectID || gotTarget != targetID {
		t.Fatalf("stored actor %s project %s target %s", gotActor, gotProject, gotTarget)
	}

	existing := NewProjects(fakeProjectStore{
		addFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (projects.Member, error) {
			return projects.Member{ProjectID: projectID, UserID: actorID, CreatedAt: created}, nil
		},
	}, time.Second).AddMember()
	selfReq, selfRec := memberRequest(projectID.String(), `{"user_id":"`+actorID.String()+`"}`)
	existing.ServeHTTP(selfRec, withUser(selfReq, actorID))
	if selfRec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", selfRec.Code, selfRec.Body.String())
	}
	const kept = `{"project_id":"22222222-2222-4222-8222-222222222222","user_id":"de498973-c030-4fc2-b075-4b5dbb421002","created_at":"2026-11-24T12:05:00Z"}`
	if selfRec.Body.String() != kept {
		t.Fatalf("body = %s", selfRec.Body.String())
	}
}

func TestAddMemberRejectsInvalidRequest(t *testing.T) {
	actorID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	tests := []struct {
		name      string
		projectID string
		body      string
	}{
		{name: "project", projectID: "not-a-uuid", body: `{"user_id":"33333333-3333-4333-8333-333333333333"}`},
		{name: "user", projectID: projectID.String(), body: `{}`},
		{name: "owner field", projectID: projectID.String(), body: `{"user_id":"33333333-3333-4333-8333-333333333333","project_id":"22222222-2222-4222-8222-222222222222"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewProjects(rejectProjectStore(t), time.Second).AddMember()
			req, rec := memberRequest(tt.projectID, tt.body)
			handler.ServeHTTP(rec, withUser(req, actorID))
			assertAPIError(t, rec, http.StatusBadRequest, CodeInvalidRequest, "The supplied request is invalid.")
		})
	}
}

func TestAddMemberNotFoundIsIdentical(t *testing.T) {
	actorID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	targetID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	handler := NewProjects(fakeProjectStore{
		addFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (projects.Member, error) {
			return projects.Member{}, projects.ErrNotFound
		},
	}, time.Second).AddMember()

	const want = `{"error":{"code":"NOT_FOUND","message":"The requested resource was not found."}}`
	for _, name := range []string{"missing project", "non-member", "missing user"} {
		t.Run(name, func(t *testing.T) {
			req, rec := memberRequest(projectID.String(), `{"user_id":"`+targetID.String()+`"}`)
			handler.ServeHTTP(rec, withUser(req, actorID))
			if rec.Code != http.StatusNotFound || rec.Body.String() != want {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAddMemberMapsPersistenceErrors(t *testing.T) {
	actorID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	targetID := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	tests := []struct {
		name    string
		err     error
		status  int
		code    Code
		message string
	}{
		{
			name:    "unavailable",
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
			handler := NewProjects(fakeProjectStore{
				addFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (projects.Member, error) {
					return projects.Member{}, tt.err
				},
			}, time.Second).AddMember()
			req, rec := memberRequest(projectID.String(), `{"user_id":"`+targetID.String()+`"}`)
			handler.ServeHTTP(rec, withUser(req, actorID))
			assertAPIError(t, rec, tt.status, tt.code, tt.message)
		})
	}
}

func TestProjectHandlersRequirePrincipal(t *testing.T) {
	handlers := NewProjects(rejectProjectStore(t), time.Second)
	memberReq, _ := memberRequest("22222222-2222-4222-8222-222222222222", `{"user_id":"33333333-3333-4333-8333-333333333333"}`)
	for _, tt := range []struct {
		name    string
		handler http.Handler
		req     *http.Request
	}{
		{name: "create", handler: handlers.Create(), req: httptest.NewRequest(http.MethodPost, "/v1/projects", nil)},
		{name: "list", handler: handlers.List(), req: httptest.NewRequest(http.MethodGet, "/v1/projects", nil)},
		{name: "member", handler: handlers.AddMember(), req: memberReq},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.handler.ServeHTTP(rec, tt.req)
			assertAPIError(t, rec, http.StatusInternalServerError, CodeInternalError, "The request could not be completed.")
		})
	}
}

func TestListProjectsMapsPersistenceErrors(t *testing.T) {
	userID := uuid.MustParse("de498973-c030-4fc2-b075-4b5dbb421002")
	handler := NewProjects(fakeProjectStore{
		listFn: func(context.Context, uuid.UUID, projects.Page) ([]db.Project, error) {
			return nil, context.DeadlineExceeded
		},
	}, time.Second).List()
	req, rec := bodyRequest(http.MethodGet, "/v1/projects", "", "")
	handler.ServeHTTP(rec, withUser(req, userID))
	assertAPIError(t, rec, http.StatusServiceUnavailable, CodeServiceUnavailable, "A required service is unavailable.")
}

func memberRequest(projectID, body string) (*http.Request, *httptest.ResponseRecorder) {
	req, rec := bodyRequest(http.MethodPost, "/v1/projects/"+projectID+"/members", "application/json", body)
	req.SetPathValue("projectID", projectID)
	return req, rec
}

type fakeProjectStore struct {
	createFn func(context.Context, uuid.UUID, string) (db.Project, error)
	listFn   func(context.Context, uuid.UUID, projects.Page) ([]db.Project, error)
	addFn    func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (projects.Member, error)
}

func (f fakeProjectStore) Create(ctx context.Context, userID uuid.UUID, name string) (db.Project, error) {
	return f.createFn(ctx, userID, name)
}

func (f fakeProjectStore) List(ctx context.Context, userID uuid.UUID, page projects.Page) ([]db.Project, error) {
	return f.listFn(ctx, userID, page)
}

func (f fakeProjectStore) AddMember(ctx context.Context, actorID, projectID, userID uuid.UUID) (projects.Member, error) {
	return f.addFn(ctx, actorID, projectID, userID)
}

func rejectProjectStore(t *testing.T) fakeProjectStore {
	t.Helper()
	return fakeProjectStore{
		createFn: func(context.Context, uuid.UUID, string) (db.Project, error) {
			t.Helper()
			t.Fatal("create called")
			return db.Project{}, nil
		},
		listFn: func(context.Context, uuid.UUID, projects.Page) ([]db.Project, error) {
			t.Helper()
			t.Fatal("list called")
			return nil, nil
		},
		addFn: func(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (projects.Member, error) {
			t.Helper()
			t.Fatal("add member called")
			return projects.Member{}, nil
		},
	}
}

func withUser(req *http.Request, userID uuid.UUID) *http.Request {
	return req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{UserID: userID}))
}
