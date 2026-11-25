// Package projects creates projects and lists the projects a user belongs to.
package projects

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smusali/sandbox-api/internal/db/sqlc"
)

// Limits are the sandbox quota and creation allowance stored on a new project.
type Limits struct {
	MaxRunningSandboxes int32
	CreationRateLimit   int32
}

// Position is the creation time and id a later page continues after.
type Position struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// Page is one membership list read.
// Limit is the number of rows to read.
// A nil After starts at the newest project.
type Page struct {
	Limit int32
	After *Position
}

// Service persists projects and the memberships that make them visible.
type Service struct {
	pool    *pgxpool.Pool
	queries *db.Queries
	limits  Limits
	timeout time.Duration
}

// New returns a service that stores limits on every created project.
// timeout bounds the rollback used when a request context is already canceled.
func New(pool *pgxpool.Pool, queries *db.Queries, limits Limits, timeout time.Duration) *Service {
	return &Service{
		pool:    pool,
		queries: queries,
		limits:  limits,
		timeout: timeout,
	}
}

// Create inserts the project and the creator membership in one transaction.
// The project is returned only after both rows commit.
func (s *Service) Create(ctx context.Context, userID uuid.UUID, name string) (db.Project, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return db.Project{}, err
	}
	defer s.rollback(tx)

	bound := s.queries.WithTx(tx)
	project, err := bound.CreateProject(ctx, db.CreateProjectParams{
		Name:                name,
		MaxRunningSandboxes: s.limits.MaxRunningSandboxes,
		CreationRateLimit:   s.limits.CreationRateLimit,
	})
	if err != nil {
		return db.Project{}, err
	}
	_, err = bound.AddProjectMember(ctx, db.AddProjectMemberParams{
		ProjectID: project.ID,
		UserID:    userID,
	})
	if err != nil {
		return db.Project{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.Project{}, err
	}
	return project, nil
}

// List returns the user's projects, newest first.
// page.Limit is the number of rows to read. A nil page.After starts at the newest project.
func (s *Service) List(ctx context.Context, userID uuid.UUID, page Page) ([]db.Project, error) {
	if page.After == nil {
		return s.queries.ListUserProjects(ctx, db.ListUserProjectsParams{
			UserID:   userID,
			RowLimit: page.Limit,
		})
	}
	return s.queries.ListUserProjectsAfter(ctx, db.ListUserProjectsAfterParams{
		UserID:          userID,
		CursorCreatedAt: page.After.CreatedAt,
		CursorID:        page.After.ID,
		RowLimit:        page.Limit,
	})
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
