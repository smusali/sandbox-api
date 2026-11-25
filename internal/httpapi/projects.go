package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/postgres"
	"github.com/smusali/sandbox-api/internal/projects"
)

type projectStore interface {
	Create(ctx context.Context, userID uuid.UUID, name string) (db.Project, error)
	List(ctx context.Context, userID uuid.UUID, page projects.Page) ([]db.Project, error)
}

var _ projectStore = (*projects.Service)(nil)

var errMissingPrincipal = errors.New("authenticated user is missing")

// Projects serves project creation and discovery.
type Projects struct {
	store   projectStore
	timeout time.Duration
}

// NewProjects binds project persistence to the HTTP handlers.
func NewProjects(store projectStore, timeout time.Duration) *Projects {
	return &Projects{store: store, timeout: timeout}
}

// Create handles POST /v1/projects.
func (p *Projects) Create() http.Handler {
	return http.HandlerFunc(p.create)
}

// List handles GET /v1/projects.
func (p *Projects) List() http.Handler {
	return http.HandlerFunc(p.list)
}

func (p *Projects) create(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUser(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	req, err := DecodeCreateProject(w, r)
	if err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	project, err := p.store.Create(ctx, userID, req.Name)
	if err != nil {
		WriteError(w, persistenceFailure(err))
		return
	}
	WriteProject(w, http.StatusCreated, NewProject(NewID(project.ID), project.Name, project.CreatedAt))
}

func (p *Projects) list(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUser(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	if err := RejectBody(w, r); err != nil {
		WriteError(w, err)
		return
	}
	page, err := ParsePage(r.URL.Query())
	if err != nil {
		WriteError(w, err)
		return
	}
	span, err := Open(page, CollectionProjects, NewID(userID))
	if err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	rows, err := p.store.List(ctx, userID, listPage(span))
	if err != nil {
		WriteError(w, persistenceFailure(err))
		return
	}
	count, more := Trim(len(rows), page.Limit)
	items := make([]Project, 0, count)
	for _, row := range rows[:count] {
		items = append(items, NewProject(NewID(row.ID), row.Name, row.CreatedAt))
	}
	var next *string
	if more {
		last := rows[count-1]
		next, err = NextCursor(CollectionProjects, NewID(userID), last.CreatedAt, NewID(last.ID), true)
		if err != nil {
			WriteError(w, err)
			return
		}
	}
	WriteProjectPage(w, http.StatusOK, NewProjectPage(items, next))
}

func principalUser(r *http.Request) (uuid.UUID, error) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		return uuid.Nil, errMissingPrincipal
	}
	return principal.UserID, nil
}

func listPage(span Span) projects.Page {
	page := projects.Page{Limit: span.Limit()}
	if !span.HasAfter() {
		return page
	}
	after := span.After()
	page.After = &projects.Position{CreatedAt: after.CreatedAt(), ID: after.ID()}
	return page
}

func persistenceFailure(err error) error {
	if postgres.Unavailable(err) {
		return ServiceUnavailable()
	}
	return err
}
