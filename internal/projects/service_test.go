package projects

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	schemadb "github.com/smusali/sandbox-api/db"
	sqldb "github.com/smusali/sandbox-api/internal/db/sqlc"
)

func TestServiceCreateAndList(t *testing.T) {
	ctx := context.Background()
	pool := newProjectDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, Limits{MaxRunningSandboxes: 10, CreationRateLimit: 20}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	empty, err := svc.List(ctx, owner.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty projects = %#v", empty)
	}

	first, err := svc.Create(ctx, owner.ID, "Agent Development")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if first.ID == uuid.Nil || first.Name != "Agent Development" || first.CreatedAt.IsZero() {
		t.Fatalf("project = %+v", first)
	}
	if first.MaxRunningSandboxes != 10 || first.CreationRateLimit != 20 {
		t.Fatalf("limits = %d, %d", first.MaxRunningSandboxes, first.CreationRateLimit)
	}
	member, err := queries.GetProjectMember(ctx, sqldb.GetProjectMemberParams{
		ProjectID: first.ID,
		UserID:    owner.ID,
	})
	if err != nil {
		t.Fatalf("GetProjectMember() error = %v", err)
	}
	if member.UserID != owner.ID || member.ProjectID != first.ID {
		t.Fatalf("membership = %+v", member)
	}

	second, err := svc.Create(ctx, owner.ID, "Agent Development")
	if err != nil {
		t.Fatalf("Create() duplicate error = %v", err)
	}
	if second.ID == uuid.Nil || second.ID == first.ID {
		t.Fatalf("duplicate id = %s", second.ID)
	}

	missingUser := uuid.New()
	if _, err := svc.Create(ctx, missingUser, "Rolled Back"); err == nil {
		t.Fatal("Create() missing user error = nil")
	}
	if n := countProjects(t, ctx, pool, "Rolled Back"); n != 0 {
		t.Fatalf("rolled back projects = %d", n)
	}
	if n := countMemberships(t, ctx, pool, missingUser); n != 0 {
		t.Fatalf("rolled back memberships = %d", n)
	}

	ordered := insertUser(t, ctx, queries, "ordered")
	older, err := svc.Create(ctx, ordered.ID, "Older")
	if err != nil {
		t.Fatalf("Create() older error = %v", err)
	}
	newer, err := svc.Create(ctx, ordered.ID, "Newer")
	if err != nil {
		t.Fatalf("Create() newer error = %v", err)
	}
	olderAt := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	newerAt := olderAt.Add(time.Minute)
	if err := setCreatedAt(ctx, pool, older.ID, olderAt); err != nil {
		t.Fatal(err)
	}
	if err := setCreatedAt(ctx, pool, newer.ID, newerAt); err != nil {
		t.Fatal(err)
	}
	page, err := svc.List(ctx, ordered.ID, Page{Limit: 1})
	if err != nil {
		t.Fatalf("List() first page error = %v", err)
	}
	if len(page) != 1 || page[0].ID != newer.ID {
		t.Fatalf("first page = %+v", page)
	}
	rest, err := svc.List(ctx, ordered.ID, Page{
		Limit: 10,
		After: &Position{CreatedAt: page[0].CreatedAt, ID: page[0].ID},
	})
	if err != nil {
		t.Fatalf("List() second page error = %v", err)
	}
	if len(rest) != 1 || rest[0].ID != older.ID {
		t.Fatalf("second page = %+v", rest)
	}

	guest := insertUser(t, ctx, queries, "guest")
	if _, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: first.ID,
		UserID:    guest.ID,
	}); err != nil {
		t.Fatalf("AddProjectMember() error = %v", err)
	}
	guestProject, err := svc.Create(ctx, guest.ID, "Guest Only")
	if err != nil {
		t.Fatalf("Create() guest error = %v", err)
	}

	ownerPage, err := svc.List(ctx, owner.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() owner error = %v", err)
	}
	ownerIDs := projectIDs(ownerPage)
	if !ownerIDs[first.ID] || !ownerIDs[second.ID] || ownerIDs[guestProject.ID] || len(ownerPage) != 2 {
		t.Fatalf("owner projects = %+v", ownerPage)
	}

	guestPage, err := svc.List(ctx, guest.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() guest error = %v", err)
	}
	guestIDs := projectIDs(guestPage)
	if !guestIDs[first.ID] || !guestIDs[guestProject.ID] || guestIDs[second.ID] || len(guestPage) != 2 {
		t.Fatalf("guest projects = %+v", guestPage)
	}

	stranger := insertUser(t, ctx, queries, "stranger")
	strangerPage, err := svc.List(ctx, stranger.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() stranger error = %v", err)
	}
	if strangerPage == nil || len(strangerPage) != 0 {
		t.Fatalf("stranger projects = %#v", strangerPage)
	}
}

func insertUser(t *testing.T, ctx context.Context, queries *sqldb.Queries, subject string) sqldb.User {
	t.Helper()
	user, err := queries.UpsertUser(ctx, sqldb.UpsertUserParams{
		OauthIssuer:  "https://issuer.example",
		OauthSubject: subject,
	})
	if err != nil {
		t.Fatalf("UpsertUser() error = %v", err)
	}
	return user
}

func countProjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	return n
}

func countMemberships(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_members WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	return n
}

func setCreatedAt(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, createdAt time.Time) error {
	_, err := pool.Exec(ctx, `UPDATE projects SET created_at = $2 WHERE id = $1`, id, createdAt)
	return err
}

func projectIDs(rows []sqldb.Project) map[uuid.UUID]bool {
	ids := make(map[uuid.UUID]bool, len(rows))
	for _, row := range rows {
		ids[row.ID] = true
	}
	return ids
}

func newProjectDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	adminURL := os.Getenv("DATABASE_URL")
	if adminURL == "" {
		adminURL = "postgres://e2b:e2b@localhost:5432/e2b"
	}

	admin, err := pgxpool.New(ctx, adminURL)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	t.Cleanup(admin.Close)

	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := admin.Ping(pingCtx); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}

	name := "sandbox_api_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+ident+" WITH (FORCE)")
	})

	parsed, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	parsed.Path = "/" + name
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := schemadb.Up(ctx, pool); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	return pool
}
