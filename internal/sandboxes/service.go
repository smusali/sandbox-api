// Package sandboxes creates running sandboxes, lists project sandbox records,
// and stops authorized sandboxes.
package sandboxes

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/postgres"
	"github.com/smusali/sandbox-api/internal/projects"
	"github.com/smusali/sandbox-api/internal/ratelimit"
)

// ErrQuotaExceeded means the locked project is already at its running sandbox limit.
var ErrQuotaExceeded = errors.New("running sandbox quota exceeded")

// RateLimitedError means admission denied this creation attempt.
// Remaining is the time left in the current window.
type RateLimitedError struct {
	Remaining time.Duration
}

func (e *RateLimitedError) Error() string {
	return "sandbox creation rate limit exceeded"
}

// Service creates, lists, and stops sandboxes for authorized project members.
type Service struct {
	pool     *pgxpool.Pool
	queries  *db.Queries
	projects *projects.Service
	admit    *ratelimit.Limiter
	timeout  time.Duration
}

// New returns a service that admits creation attempts through admit.
// projects authorizes the caller before admission or listing.
// timeout bounds the rollback used when a request context is already canceled.
func New(pool *pgxpool.Pool, queries *db.Queries, projects *projects.Service, admit *ratelimit.Limiter, timeout time.Duration) *Service {
	return &Service{
		pool:     pool,
		queries:  queries,
		projects: projects,
		admit:    admit,
		timeout:  timeout,
	}
}

// Create inserts one running sandbox for a member of projectID.
// Membership is confirmed before rate admission. Redis admission finishes
// before the project row is locked. Quota uses the locked project's limit
// and a separate count of running sandboxes. The row is returned only after
// commit. A failed commit is not retried, because another request may create
// a different sandbox. An admitted rate unit stays consumed when quota or
// persistence fails.
func (s *Service) Create(ctx context.Context, userID, projectID uuid.UUID) (db.Sandbox, error) {
	project, err := s.projects.GetAuthorizedProject(ctx, projectID, userID)
	if err != nil {
		return db.Sandbox{}, err
	}

	decision, err := s.admit.Allow(ctx, project.ID, project.CreationRateLimit)
	if err != nil {
		return db.Sandbox{}, err
	}
	if !decision.Allowed {
		return db.Sandbox{}, &RateLimitedError{Remaining: decision.Remaining}
	}

	var created db.Sandbox
	err = s.withLockedProject(ctx, project.ID, userID, func(ctx context.Context, q *db.Queries, locked db.Project) error {
		running, err := q.CountRunningSandboxes(ctx, locked.ID)
		if err != nil {
			return err
		}
		if quotaFull(running, locked.MaxRunningSandboxes) {
			return ErrQuotaExceeded
		}
		created, err = q.CreateSandbox(ctx, locked.ID)
		return err
	})
	if err != nil {
		return db.Sandbox{}, err
	}
	return created, nil
}

// List returns one page of the project's sandboxes, newest first.
// The caller must be a member. Running and stopped rows are included.
// page.Limit is the number of rows to read. A nil page.After starts at the newest sandbox.
// Listing does not consume creation allowance and does not lock the project.
func (s *Service) List(ctx context.Context, userID, projectID uuid.UUID, page postgres.Page) ([]db.Sandbox, error) {
	if _, err := s.projects.GetAuthorizedProject(ctx, projectID, userID); err != nil {
		return nil, err
	}
	if page.After == nil {
		return s.queries.ListProjectSandboxes(ctx, db.ListProjectSandboxesParams{
			ProjectID: projectID,
			RowLimit:  page.Limit,
		})
	}
	return s.queries.ListProjectSandboxesAfter(ctx, db.ListProjectSandboxesAfterParams{
		ProjectID:       projectID,
		CursorCreatedAt: page.After.CreatedAt,
		CursorID:        page.After.ID,
		RowLimit:        page.Limit,
	})
}

// Stop transitions an authorized sandbox from running to stopped.
// The project id comes from the persisted sandbox. Membership is confirmed
// again while that project row is locked, in the same order as creation.
// A row that is already stopped commits unchanged, so the original stopped_at
// stays and no extra capacity is released. Termination does not call Redis
// and does not adjust a separate quota counter. Occupancy is the committed
// count of running rows. A failed commit leaves that count unchanged.
func (s *Service) Stop(ctx context.Context, userID, sandboxID uuid.UUID) error {
	sandbox, err := s.projects.GetAuthorizedSandbox(ctx, sandboxID, userID)
	if err != nil {
		return err
	}
	return s.withLockedProject(ctx, sandbox.ProjectID, userID, func(ctx context.Context, q *db.Queries, locked db.Project) error {
		_, err := q.StopRunningSandbox(ctx, db.StopRunningSandboxParams{
			ID:        sandbox.ID,
			ProjectID: locked.ID,
		})
		return err
	})
}

// withLockedProject runs fn while the caller's project row is locked.
// The running count stays outside the lock statement. fn must not call Redis.
func (s *Service) withLockedProject(ctx context.Context, projectID, userID uuid.UUID, fn func(context.Context, *db.Queries, db.Project) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx, s.timeout)

	bound := s.queries.WithTx(tx)
	locked, err := bound.LockAuthorizedProject(ctx, db.LockAuthorizedProjectParams{
		ProjectID: projectID,
		UserID:    userID,
	})
	if err != nil {
		return postgres.Absent(err)
	}
	if err := fn(ctx, bound, locked); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// quotaFull reports that running occupancy is already at limit.
// A non-positive limit fails closed.
func quotaFull(running int64, limit int32) bool {
	if running < 0 || limit < 1 {
		return true
	}
	return running >= runningCapacity(limit)
}

func runningCapacity(limit int32) int64 {
	return int64(limit)
}
