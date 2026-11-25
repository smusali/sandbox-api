package db

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	sqldb "github.com/smusali/sandbox-api/internal/db/sqlc"
)

func TestSchema(t *testing.T) {
	ctx := context.Background()
	pool := newSchemaDB(t)
	queries := sqldb.New(pool)

	t.Run("constraints", func(t *testing.T) {
		testConstraints(t, ctx, pool)
	})
	t.Run("indexes", func(t *testing.T) {
		testIndexes(t, ctx, pool)
	})
	t.Run("queries", func(t *testing.T) {
		testQueries(t, ctx, queries, pool)
	})

	if err := Down(ctx, pool); err != nil {
		t.Fatalf("Down() error = %v", err)
	}
	var tables []string
	rows, err := pool.Query(ctx, `
		SELECT table_name
		FROM information_schema.tables
		WHERE table_schema = 'public'
		  AND table_name = ANY($1)
	`, []string{"users", "projects", "project_members", "sandboxes"})
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("tables: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("tables after Down = %v", tables)
	}
}

func newSchemaDB(t *testing.T) *pgxpool.Pool {
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

	if err := Up(ctx, pool); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	return pool
}

func testConstraints(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	var userID, projectID uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO users (oauth_issuer, oauth_subject, email)
		VALUES ('https://issuer.example', 'subject-1', 'one@example.com')
		RETURNING id
	`).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	err = pool.QueryRow(ctx, `
		INSERT INTO projects (name, max_running_sandboxes, creation_rate_limit)
		VALUES ('Constraint Project', 2, 3)
		RETURNING id
	`).Scan(&projectID)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO project_members (project_id, user_id) VALUES ($1, $2)
	`, projectID, userID); err != nil {
		t.Fatalf("insert membership: %v", err)
	}

	expectSQLState(t, reject(ctx, pool, `
		INSERT INTO users (oauth_issuer, oauth_subject)
		VALUES ('https://issuer.example', 'subject-1')
	`), "23505")
	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO project_members (project_id, user_id) VALUES ($1, $2)
	`, projectID, userID), "23505")

	missingID := uuid.New()
	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO sandboxes (project_id) VALUES ($1)
	`, missingID), "23503")
	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO project_members (project_id, user_id) VALUES ($1, $2)
	`, projectID, missingID), "23503")
	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO project_members (project_id, user_id) VALUES ($1, $2)
	`, missingID, userID), "23503")

	rejections := []string{
		`INSERT INTO users (oauth_issuer, oauth_subject) VALUES ('', 'subject')`,
		`INSERT INTO users (oauth_issuer, oauth_subject) VALUES ('https://issuer.example', '')`,
		`INSERT INTO projects (name, max_running_sandboxes, creation_rate_limit) VALUES ('', 1, 1)`,
		`INSERT INTO projects (name, max_running_sandboxes, creation_rate_limit) VALUES ('   ', 1, 1)`,
		`INSERT INTO projects (name, max_running_sandboxes, creation_rate_limit) VALUES ('Limits', 0, 1)`,
		`INSERT INTO projects (name, max_running_sandboxes, creation_rate_limit) VALUES ('Limits', 1, 0)`,
		`INSERT INTO projects (name, max_running_sandboxes, creation_rate_limit) VALUES ('Limits', -1, 1)`,
	}
	for _, statement := range rejections {
		expectSQLState(t, reject(ctx, pool, statement), "23514")
	}

	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO sandboxes (project_id, status) VALUES ($1, 'deleted')
	`, projectID), "23514")
	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO sandboxes (project_id, status, stopped_at)
		VALUES ($1, 'running', now())
	`, projectID), "23514")
	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO sandboxes (project_id, status)
		VALUES ($1, 'stopped')
	`, projectID), "23514")
	expectSQLState(t, rejectID(ctx, pool, `
		INSERT INTO sandboxes (project_id, status, created_at, stopped_at)
		VALUES ($1, 'stopped', now(), now() - interval '1 second')
	`, projectID), "23514")
}

func testIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	rows, err := pool.Query(ctx, `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = 'public'
		  AND tablename <> 'goose_db_version'
	`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()

	defs := map[string]string{}
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		defs[name] = def
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("indexes: %v", err)
	}

	want := []string{
		"users_pkey",
		"users_external_identity_unique",
		"projects_pkey",
		"project_members_pkey",
		"idx_project_members_user_project",
		"sandboxes_pkey",
		"idx_sandboxes_project_created",
		"idx_sandboxes_running_project",
	}
	if len(defs) != len(want) {
		t.Fatalf("indexes = %#v", defs)
	}
	for _, name := range want {
		if _, ok := defs[name]; !ok {
			t.Fatalf("missing index %s in %#v", name, defs)
		}
	}
	if !strings.Contains(defs["idx_sandboxes_project_created"], "created_at DESC") ||
		!strings.Contains(defs["idx_sandboxes_project_created"], "id DESC") {
		t.Fatalf("project created index = %s", defs["idx_sandboxes_project_created"])
	}
	running := defs["idx_sandboxes_running_project"]
	if !strings.Contains(running, "WHERE") || !strings.Contains(running, "running") {
		t.Fatalf("running index = %s", running)
	}
	members := defs["idx_project_members_user_project"]
	if !strings.Contains(members, "user_id") || !strings.Contains(members, "project_id") {
		t.Fatalf("membership index = %s", members)
	}
}

func testQueries(t *testing.T, ctx context.Context, queries *sqldb.Queries, pool *pgxpool.Pool) {
	t.Helper()

	email := "foo@bar.com"
	issuer := "http://localhost:4444"
	subject := "foo@bar.com"
	created, err := queries.UpsertUser(ctx, sqldb.UpsertUserParams{
		OauthIssuer:  issuer,
		OauthSubject: subject,
		Email:        &email,
	})
	if err != nil {
		t.Fatalf("UpsertUser() error = %v", err)
	}

	kept, err := queries.UpsertUser(ctx, sqldb.UpsertUserParams{
		OauthIssuer:  issuer,
		OauthSubject: subject,
	})
	if err != nil {
		t.Fatalf("UpsertUser() again error = %v", err)
	}
	if kept.ID != created.ID || !kept.CreatedAt.Equal(created.CreatedAt) {
		t.Fatalf("repeated upsert = %+v, want id %s created %s", kept, created.ID, created.CreatedAt)
	}
	if kept.Email == nil || *kept.Email != email {
		t.Fatalf("email after null upsert = %v", kept.Email)
	}

	changed := "other@bar.com"
	renamed, err := queries.UpsertUser(ctx, sqldb.UpsertUserParams{
		OauthIssuer:  issuer,
		OauthSubject: subject,
		Email:        &changed,
	})
	if err != nil {
		t.Fatalf("UpsertUser() email error = %v", err)
	}
	if renamed.ID != created.ID || renamed.Email == nil || *renamed.Email != changed {
		t.Fatalf("email update = %+v", renamed)
	}

	byIdentity, err := queries.GetUserByIdentity(ctx, sqldb.GetUserByIdentityParams{
		OauthIssuer:  issuer,
		OauthSubject: subject,
	})
	if err != nil {
		t.Fatalf("GetUserByIdentity() error = %v", err)
	}
	if byIdentity.ID != created.ID {
		t.Fatalf("identity id = %s", byIdentity.ID)
	}
	byID, err := queries.GetUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetUserByID() error = %v", err)
	}
	if byID.OauthSubject != subject {
		t.Fatalf("subject = %s", byID.OauthSubject)
	}

	first, err := queries.CreateProject(ctx, sqldb.CreateProjectParams{
		Name:                "Older",
		MaxRunningSandboxes: 10,
		CreationRateLimit:   20,
	})
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	second, err := queries.CreateProject(ctx, sqldb.CreateProjectParams{
		Name:                "Newer",
		MaxRunningSandboxes: 4,
		CreationRateLimit:   5,
	})
	if err != nil {
		t.Fatalf("CreateProject() second error = %v", err)
	}

	member, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: first.ID,
		UserID:    created.ID,
	})
	if err != nil {
		t.Fatalf("AddProjectMember() error = %v", err)
	}
	_, err = queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: first.ID,
		UserID:    created.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate AddProjectMember() error = %v", err)
	}
	persisted, err := queries.GetProjectMember(ctx, sqldb.GetProjectMemberParams{
		ProjectID: first.ID,
		UserID:    created.ID,
	})
	if err != nil {
		t.Fatalf("GetProjectMember() error = %v", err)
	}
	if !persisted.CreatedAt.Equal(member.CreatedAt) {
		t.Fatalf("membership timestamp = %s, want %s", persisted.CreatedAt, member.CreatedAt)
	}
	if _, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: second.ID,
		UserID:    created.ID,
	}); err != nil {
		t.Fatalf("AddProjectMember() second error = %v", err)
	}

	older := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Minute)
	if err := setProjectCreatedAt(ctx, pool, first.ID, older); err != nil {
		t.Fatal(err)
	}
	if err := setProjectCreatedAt(ctx, pool, second.ID, newer); err != nil {
		t.Fatal(err)
	}

	page, err := queries.ListUserProjects(ctx, sqldb.ListUserProjectsParams{
		UserID:   created.ID,
		RowLimit: 1,
	})
	if err != nil {
		t.Fatalf("ListUserProjects() error = %v", err)
	}
	if len(page) != 1 || page[0].ID != second.ID {
		t.Fatalf("first project page = %+v", page)
	}
	rest, err := queries.ListUserProjectsAfter(ctx, sqldb.ListUserProjectsAfterParams{
		UserID:          created.ID,
		CursorCreatedAt: page[0].CreatedAt,
		CursorID:        page[0].ID,
		RowLimit:        10,
	})
	if err != nil {
		t.Fatalf("ListUserProjectsAfter() error = %v", err)
	}
	if len(rest) != 1 || rest[0].ID != first.ID {
		t.Fatalf("second project page = %+v", rest)
	}

	stranger, err := queries.UpsertUser(ctx, sqldb.UpsertUserParams{
		OauthIssuer:  issuer,
		OauthSubject: "stranger",
	})
	if err != nil {
		t.Fatalf("UpsertUser() stranger error = %v", err)
	}
	empty, err := queries.ListUserProjects(ctx, sqldb.ListUserProjectsParams{
		UserID:   stranger.ID,
		RowLimit: 50,
	})
	if err != nil {
		t.Fatalf("ListUserProjects() empty error = %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty projects = %#v", empty)
	}

	authorized, err := queries.GetAuthorizedProject(ctx, sqldb.GetAuthorizedProjectParams{
		ProjectID: second.ID,
		UserID:    created.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedProject() error = %v", err)
	}
	if authorized.MaxRunningSandboxes != 4 || authorized.CreationRateLimit != 5 {
		t.Fatalf("limits = %d, %d", authorized.MaxRunningSandboxes, authorized.CreationRateLimit)
	}
	_, err = queries.GetAuthorizedProject(ctx, sqldb.GetAuthorizedProjectParams{
		ProjectID: second.ID,
		UserID:    stranger.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unauthorized project error = %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	bound := queries.WithTx(tx)
	locked, err := bound.LockAuthorizedProject(ctx, sqldb.LockAuthorizedProjectParams{
		ProjectID: second.ID,
		UserID:    created.ID,
	})
	if err != nil {
		t.Fatalf("LockAuthorizedProject() error = %v", err)
	}
	if locked.ID != second.ID {
		t.Fatalf("locked project = %s", locked.ID)
	}
	occupied, err := bound.CountRunningSandboxes(ctx, second.ID)
	if err != nil {
		t.Fatalf("CountRunningSandboxes() error = %v", err)
	}
	if occupied != 0 {
		t.Fatalf("running count = %d", occupied)
	}
	if _, err := bound.CreateSandbox(ctx, second.ID); err != nil {
		t.Fatalf("CreateSandbox() in transaction error = %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	occupied, err = queries.CountRunningSandboxes(ctx, second.ID)
	if err != nil {
		t.Fatalf("CountRunningSandboxes() after rollback error = %v", err)
	}
	if occupied != 0 {
		t.Fatalf("running count after rollback = %d", occupied)
	}

	running, err := queries.CreateSandbox(ctx, second.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	if running.Status != "running" || running.StoppedAt != nil || running.ProjectID != second.ID {
		t.Fatalf("created sandbox = %+v", running)
	}
	visible, err := queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: running.ID,
		UserID:    created.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedSandbox() error = %v", err)
	}
	if visible.ID != running.ID {
		t.Fatalf("sandbox id = %s", visible.ID)
	}
	_, err = queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: running.ID,
		UserID:    stranger.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unauthorized sandbox error = %v", err)
	}

	stoppedSandbox, err := queries.CreateSandbox(ctx, second.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() stopped error = %v", err)
	}
	affected, err := queries.StopRunningSandbox(ctx, sqldb.StopRunningSandboxParams{
		ID:        stoppedSandbox.ID,
		ProjectID: second.ID,
	})
	if err != nil {
		t.Fatalf("StopRunningSandbox() error = %v", err)
	}
	if affected != 1 {
		t.Fatalf("stopped rows = %d", affected)
	}
	stopped, err := queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: stoppedSandbox.ID,
		UserID:    created.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedSandbox() stopped error = %v", err)
	}
	if stopped.Status != "stopped" || stopped.StoppedAt == nil || stopped.StoppedAt.Before(stopped.CreatedAt) {
		t.Fatalf("stopped sandbox = %+v", stopped)
	}
	affected, err = queries.StopRunningSandbox(ctx, sqldb.StopRunningSandboxParams{
		ID:        stoppedSandbox.ID,
		ProjectID: second.ID,
	})
	if err != nil {
		t.Fatalf("StopRunningSandbox() repeat error = %v", err)
	}
	if affected != 0 {
		t.Fatalf("repeat stopped rows = %d", affected)
	}
	repeated, err := queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: stoppedSandbox.ID,
		UserID:    created.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedSandbox() repeat error = %v", err)
	}
	if repeated.StoppedAt == nil || !repeated.StoppedAt.Equal(*stopped.StoppedAt) {
		t.Fatalf("stopped_at changed from %v to %v", stopped.StoppedAt, repeated.StoppedAt)
	}

	if err := setSandboxCreatedAt(ctx, pool, running.ID, newer); err != nil {
		t.Fatal(err)
	}
	if err := setSandboxCreatedAt(ctx, pool, stoppedSandbox.ID, older); err != nil {
		t.Fatal(err)
	}
	sandboxPage, err := queries.ListProjectSandboxes(ctx, sqldb.ListProjectSandboxesParams{
		ProjectID: second.ID,
		RowLimit:  1,
	})
	if err != nil {
		t.Fatalf("ListProjectSandboxes() error = %v", err)
	}
	if len(sandboxPage) != 1 || sandboxPage[0].ID != running.ID || sandboxPage[0].Status != "running" {
		t.Fatalf("first sandbox page = %+v", sandboxPage)
	}
	sandboxRest, err := queries.ListProjectSandboxesAfter(ctx, sqldb.ListProjectSandboxesAfterParams{
		ProjectID:       second.ID,
		CursorCreatedAt: sandboxPage[0].CreatedAt,
		CursorID:        sandboxPage[0].ID,
		RowLimit:        10,
	})
	if err != nil {
		t.Fatalf("ListProjectSandboxesAfter() error = %v", err)
	}
	if len(sandboxRest) != 1 || sandboxRest[0].ID != stoppedSandbox.ID || sandboxRest[0].Status != "stopped" {
		t.Fatalf("second sandbox page = %+v", sandboxRest)
	}
	none, err := queries.ListProjectSandboxes(ctx, sqldb.ListProjectSandboxesParams{
		ProjectID: first.ID,
		RowLimit:  50,
	})
	if err != nil {
		t.Fatalf("ListProjectSandboxes() empty error = %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Fatalf("empty sandboxes = %#v", none)
	}

	occupied, err = queries.CountRunningSandboxes(ctx, second.ID)
	if err != nil {
		t.Fatalf("CountRunningSandboxes() final error = %v", err)
	}
	if occupied != 1 {
		t.Fatalf("final running count = %d", occupied)
	}
}

func setProjectCreatedAt(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, createdAt time.Time) error {
	_, err := pool.Exec(ctx, `UPDATE projects SET created_at = $2 WHERE id = $1`, id, createdAt)
	return err
}

func setSandboxCreatedAt(ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, createdAt time.Time) error {
	_, err := pool.Exec(ctx, `
		UPDATE sandboxes
		SET created_at = $2,
		    stopped_at = CASE
		        WHEN status = 'stopped' THEN GREATEST(stopped_at, $2)
		        ELSE stopped_at
		    END
		WHERE id = $1
	`, id, createdAt)
	return err
}

func reject(ctx context.Context, pool *pgxpool.Pool, statement string) error {
	_, err := pool.Exec(ctx, statement)
	return err
}

func rejectID(ctx context.Context, pool *pgxpool.Pool, statement string, ids ...uuid.UUID) error {
	switch len(ids) {
	case 1:
		_, err := pool.Exec(ctx, statement, ids[0])
		return err
	case 2:
		_, err := pool.Exec(ctx, statement, ids[0], ids[1])
		return err
	default:
		return errors.New("rejectID requires one or two ids")
	}
}

func expectSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
	if pgErr.Code != code {
		t.Fatalf("SQLSTATE = %s (%s), want %s", pgErr.Code, pgErr.Message, code)
	}
}
