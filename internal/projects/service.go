// Package projects creates projects, records membership, and authorizes access.
package projects

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/postgres"
)

// errMembershipMissing is an insert conflict whose membership row cannot be read.
var errMembershipMissing = errors.New("membership row missing after insert conflict")

// Limits are the sandbox quota and creation allowance stored on a new project.
type Limits struct {
	MaxRunningSandboxes int32
	CreationRateLimit   int32
}

// Member is one project membership.
// Created is true only when this call inserted the row.
type Member struct {
	ProjectID uuid.UUID
	UserID    uuid.UUID
	CreatedAt time.Time
	Created   bool
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
	defer postgres.Rollback(tx, s.timeout)

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
func (s *Service) List(ctx context.Context, userID uuid.UUID, page postgres.Page) ([]db.Project, error) {
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

// GetAuthorizedProject returns the project when userID is a member.
// A missing project and a project without that membership both return postgres.ErrNotFound.
// The result includes the project's persisted sandbox limits.
func (s *Service) GetAuthorizedProject(ctx context.Context, projectID, userID uuid.UUID) (db.Project, error) {
	project, err := s.queries.GetAuthorizedProject(ctx, db.GetAuthorizedProjectParams{
		ProjectID: projectID,
		UserID:    userID,
	})
	if err != nil {
		return db.Project{}, postgres.Absent(err)
	}
	return project, nil
}

// GetAuthorizedSandbox returns the sandbox when userID belongs to its stored project.
// The sandbox id is the only resource identifier. A missing sandbox and a sandbox
// in another project both return postgres.ErrNotFound.
func (s *Service) GetAuthorizedSandbox(ctx context.Context, sandboxID, userID uuid.UUID) (db.Sandbox, error) {
	sandbox, err := s.queries.GetAuthorizedSandbox(ctx, db.GetAuthorizedSandboxParams{
		SandboxID: sandboxID,
		UserID:    userID,
	})
	if err != nil {
		return db.Sandbox{}, postgres.Absent(err)
	}
	return sandbox, nil
}

// AddMember inserts a membership for an existing user.
// The actor's membership is confirmed before the target user is read.
// A new row has Created set. An existing row, including the actor, keeps its timestamp.
func (s *Service) AddMember(ctx context.Context, actorID, projectID, userID uuid.UUID) (Member, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Member{}, err
	}
	defer postgres.Rollback(tx, s.timeout)

	bound := s.queries.WithTx(tx)
	if _, err := bound.GetAuthorizedProject(ctx, db.GetAuthorizedProjectParams{
		ProjectID: projectID,
		UserID:    actorID,
	}); err != nil {
		return Member{}, postgres.Absent(err)
	}
	if _, err := bound.GetUserByID(ctx, userID); err != nil {
		return Member{}, postgres.Absent(err)
	}

	inserted, err := bound.AddProjectMember(ctx, db.AddProjectMemberParams{
		ProjectID: projectID,
		UserID:    userID,
	})
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return Member{}, err
		}
		return memberFrom(inserted, true), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Member{}, err
	}

	existing, err := bound.GetProjectMember(ctx, db.GetProjectMemberParams{
		ProjectID: projectID,
		UserID:    userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Member{}, errMembershipMissing
	}
	if err != nil {
		return Member{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Member{}, err
	}
	return memberFrom(existing, false), nil
}

func memberFrom(row db.ProjectMember, created bool) Member {
	return Member{
		ProjectID: row.ProjectID,
		UserID:    row.UserID,
		CreatedAt: row.CreatedAt,
		Created:   created,
	}
}
