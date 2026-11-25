package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/postgres"
	"github.com/smusali/sandbox-api/internal/ratelimit"
	"github.com/smusali/sandbox-api/internal/sandboxes"
)

type sandboxStore interface {
	Create(ctx context.Context, userID, projectID uuid.UUID) (db.Sandbox, error)
	List(ctx context.Context, userID, projectID uuid.UUID, page sandboxes.Page) ([]db.Sandbox, error)
}

var _ sandboxStore = (*sandboxes.Service)(nil)

// Sandboxes serves sandbox creation and listing.
type Sandboxes struct {
	store   sandboxStore
	timeout time.Duration
}

// NewSandboxes binds sandbox persistence to the HTTP handlers.
func NewSandboxes(store sandboxStore, timeout time.Duration) *Sandboxes {
	return &Sandboxes{store: store, timeout: timeout}
}

// Create handles POST /v1/projects/{projectID}/sandboxes.
func (s *Sandboxes) Create() http.Handler {
	return http.HandlerFunc(s.create)
}

// List handles GET /v1/projects/{projectID}/sandboxes.
func (s *Sandboxes) List() http.Handler {
	return http.HandlerFunc(s.list)
}

func (s *Sandboxes) create(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUser(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	projectID, err := PathID(r, "projectID")
	if err != nil {
		WriteError(w, err)
		return
	}
	if err := DecodeCreateSandbox(w, r); err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	sandbox, err := s.store.Create(ctx, userID, projectID.UUID())
	if err != nil {
		WriteError(w, creationFailure(err))
		return
	}
	WriteSandbox(w, http.StatusCreated, sandboxResponse(sandbox))
}

func (s *Sandboxes) list(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUser(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	projectID, err := PathID(r, "projectID")
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
	span, err := Open(page, CollectionSandboxes, projectID)
	if err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	rows, err := s.store.List(ctx, userID, projectID.UUID(), sandboxPage(span))
	if err != nil {
		WriteError(w, listFailure(err))
		return
	}
	count, more := Trim(len(rows), page.Limit)
	items := make([]Sandbox, 0, count)
	for _, row := range rows[:count] {
		items = append(items, sandboxResponse(row))
	}
	var next *string
	if more {
		last := rows[count-1]
		next, err = NextCursor(CollectionSandboxes, projectID, last.CreatedAt, NewID(last.ID), true)
		if err != nil {
			WriteError(w, err)
			return
		}
	}
	WriteSandboxPage(w, http.StatusOK, NewSandboxPage(items, next))
}

func sandboxResponse(row db.Sandbox) Sandbox {
	return NewSandbox(
		NewID(row.ID),
		NewID(row.ProjectID),
		row.Status,
		row.CreatedAt,
		row.StoppedAt,
	)
}

func sandboxPage(span Span) sandboxes.Page {
	page := sandboxes.Page{Limit: span.Limit()}
	if !span.HasAfter() {
		return page
	}
	after := span.After()
	page.After = &sandboxes.Position{CreatedAt: after.CreatedAt(), ID: after.ID()}
	return page
}

func listFailure(err error) error {
	if errors.Is(err, sandboxes.ErrNotFound) {
		return NotFound()
	}
	return persistenceFailure(err)
}

func creationFailure(err error) error {
	if errors.Is(err, sandboxes.ErrNotFound) {
		return NotFound()
	}
	var limited *sandboxes.RateLimitedError
	if errors.As(err, &limited) && limited != nil {
		return RateLimited(limited.Remaining)
	}
	if errors.Is(err, sandboxes.ErrQuotaExceeded) {
		return QuotaExceeded()
	}
	if errors.Is(err, ratelimit.ErrUnavailable) || postgres.Unavailable(err) {
		return ServiceUnavailable()
	}
	return err
}
