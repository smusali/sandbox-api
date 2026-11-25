package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/projects"
)

var errMissingPrincipal = errors.New("authenticated user is missing")

// Projects serves project creation, discovery, and membership.
type Projects struct {
	store   *projects.Service
	timeout time.Duration
}

// NewProjects binds project persistence to the HTTP handlers.
func NewProjects(store *projects.Service, timeout time.Duration) *Projects {
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

// AddMember handles POST /v1/projects/{projectID}/members.
func (p *Projects) AddMember() http.Handler {
	return http.HandlerFunc(p.addMember)
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
	rows, err := p.store.List(ctx, userID, queryPage(span))
	if err != nil {
		WriteError(w, resourceFailure(err))
		return
	}
	end, err := endPage(CollectionProjects, NewID(userID), len(rows), page.Limit, func(i int) (time.Time, ID) {
		return rows[i].CreatedAt, NewID(rows[i].ID)
	})
	if err != nil {
		WriteError(w, err)
		return
	}
	items := make([]Project, 0, end.count)
	for _, row := range rows[:end.count] {
		items = append(items, NewProject(NewID(row.ID), row.Name, row.CreatedAt))
	}
	WriteProjectPage(w, http.StatusOK, NewProjectPage(items, end.next))
}

func (p *Projects) addMember(w http.ResponseWriter, r *http.Request) {
	actorID, err := principalUser(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	projectID, err := PathID(r, "projectID")
	if err != nil {
		WriteError(w, err)
		return
	}
	req, err := DecodeAddMember(w, r)
	if err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	member, err := p.store.AddMember(ctx, actorID, projectID.UUID(), req.UserID.UUID())
	if err != nil {
		WriteError(w, resourceFailure(err))
		return
	}
	status := http.StatusOK
	if member.Created {
		status = http.StatusCreated
	}
	WriteMembership(w, status, NewMembership(NewID(member.ProjectID), NewID(member.UserID), member.CreatedAt))
}

func principalUser(r *http.Request) (uuid.UUID, error) {
	principal, ok := auth.PrincipalFrom(r.Context())
	if !ok {
		return uuid.Nil, errMissingPrincipal
	}
	return principal.UserID, nil
}
