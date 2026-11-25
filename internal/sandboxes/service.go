// Package sandboxes creates running sandboxes under a project's rate allowance and quota.
package sandboxes

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/ratelimit"
)

// ErrNotFound means the project is missing or the user is not a member.
// Callers must treat every cause as the same result.
var ErrNotFound = errors.New("not found")

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

// admitter counts one creation attempt for a project.
// An admitted attempt is not returned after quota or persistence failure.
type admitter interface {
	Allow(ctx context.Context, projectID uuid.UUID, limit int32) (ratelimit.Decision, error)
}

var _ admitter = (*ratelimit.Limiter)(nil)

// Service persists running sandboxes for authorized project members.
type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	admit   admitter
	timeout time.Duration
}

// New returns a service that admits creation attempts through admit.
// timeout bounds the rollback used when a request context is already canceled.
func New(pool *pgxpool.Pool, queries *db.Queries, admit admitter, timeout time.Duration) *Service {
	return &Service{
		pool:    pool,
		queries: queries,
		admit:   admit,
		timeout: timeout,
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
	project, err := s.queries.GetAuthorizedProject(ctx, db.GetAuthorizedProjectParams{
		ProjectID: projectID,
		UserID:    userID,
	})
	if err != nil {
		return db.Sandbox{}, absent(err)
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

// withLockedProject runs fn while the caller's project row is locked.
// The running count stays outside the lock statement. fn must not call Redis.
func (s *Service) withLockedProject(ctx context.Context, projectID, userID uuid.UUID, fn func(context.Context, *db.Queries, db.Project) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer s.rollback(tx)

	bound := s.queries.WithTx(tx)
	locked, err := bound.LockAuthorizedProject(ctx, db.LockAuthorizedProjectParams{
		ProjectID: projectID,
		UserID:    userID,
	})
	if err != nil {
		return absent(err)
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

func absent(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Service) rollback(tx pgx.Tx) {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	err := tx.Rollback(rollbackCtx)
	if err == nil || errors.Is(err, pgx.ErrTxClosed) {
		return
	}
	log.Printf("internal error: %v", err)
}
