package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/ratelimit"
	"github.com/smusali/sandbox-api/internal/sandboxes"
)

// Sandboxes serves sandbox creation, listing, and termination.
type Sandboxes struct {
	store   *sandboxes.Service
	timeout time.Duration
}

// NewSandboxes binds sandbox persistence to the HTTP handlers.
func NewSandboxes(store *sandboxes.Service, timeout time.Duration) *Sandboxes {
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

// Stop handles DELETE /v1/sandboxes/{sandboxID}.
func (s *Sandboxes) Stop() http.Handler {
	return http.HandlerFunc(s.stop)
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
	rows, err := s.store.List(ctx, userID, projectID.UUID(), queryPage(span))
	if err != nil {
		WriteError(w, resourceFailure(err))
		return
	}
	end, err := endPage(CollectionSandboxes, projectID, len(rows), page.Limit, func(i int) (time.Time, ID) {
		return rows[i].CreatedAt, NewID(rows[i].ID)
	})
	if err != nil {
		WriteError(w, err)
		return
	}
	items := make([]Sandbox, 0, end.count)
	for _, row := range rows[:end.count] {
		items = append(items, sandboxResponse(row))
	}
	WriteSandboxPage(w, http.StatusOK, NewSandboxPage(items, end.next))
}

func (s *Sandboxes) stop(w http.ResponseWriter, r *http.Request) {
	userID, err := principalUser(r)
	if err != nil {
		WriteError(w, err)
		return
	}
	sandboxID, err := PathID(r, "sandboxID")
	if err != nil {
		WriteError(w, err)
		return
	}
	if err := RejectBody(w, r); err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	if err := s.store.Stop(ctx, userID, sandboxID.UUID()); err != nil {
		WriteError(w, resourceFailure(err))
		return
	}
	WriteNoContent(w)
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

func creationFailure(err error) error {
	var limited *sandboxes.RateLimitedError
	if errors.As(err, &limited) && limited != nil {
		return RateLimited(limited.Remaining)
	}
	if errors.Is(err, sandboxes.ErrQuotaExceeded) {
		return QuotaExceeded()
	}
	if errors.Is(err, ratelimit.ErrUnavailable) {
		return ServiceUnavailable()
	}
	return resourceFailure(err)
}
