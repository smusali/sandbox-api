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
}

var _ sandboxStore = (*sandboxes.Service)(nil)

// Sandboxes serves sandbox creation.
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
	WriteSandbox(w, http.StatusCreated, NewSandbox(
		NewID(sandbox.ID),
		NewID(sandbox.ProjectID),
		sandbox.Status,
		sandbox.CreatedAt,
		sandbox.StoppedAt,
	))
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
