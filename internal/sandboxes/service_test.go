package sandboxes

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	schemadb "github.com/smusali/sandbox-api/db"
	sqldb "github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/ratelimit"
)

func TestCreateRunningSandbox(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Agent", 10, 7)

	first, err := svc.Create(ctx, owner.ID, project.ID)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if first.ID == uuid.Nil || first.ProjectID != project.ID || first.Status != "running" || first.StoppedAt != nil || first.CreatedAt.IsZero() {
		t.Fatalf("sandbox = %+v", first)
	}

	second, err := svc.Create(ctx, owner.ID, project.ID)
	if err != nil {
		t.Fatalf("Create() second error = %v", err)
	}
	if second.ID == uuid.Nil || second.ID == first.ID {
		t.Fatalf("second id = %s", second.ID)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 2 {
		t.Fatalf("running = %d", running)
	}
	calls := admit.snapshot()
	if len(calls) != 2 || calls[0].projectID != project.ID || calls[0].limit != 7 {
		t.Fatalf("admission = %+v", calls)
	}
}

func TestCreateRejectsUnauthorizedBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	stranger := insertUser(t, ctx, queries, "stranger")
	project := insertProject(t, ctx, queries, owner.ID, "Private", 10, 20)

	if _, err := svc.Create(ctx, stranger.ID, project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger error = %v", err)
	}
	if _, err := svc.Create(ctx, owner.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project error = %v", err)
	}
	if calls := admit.snapshot(); len(calls) != 0 {
		t.Fatalf("admission = %+v", calls)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running = %d", running)
	}
}

func TestCreateRateLimitBeforeQuota(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{allow: denyAfter(24 * time.Second)}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Full", 1, 20)
	seedRunning(t, ctx, queries, project.ID, 1)

	_, err := svc.Create(ctx, owner.ID, project.ID)
	var limited *RateLimitedError
	if !errors.As(err, &limited) || limited.Remaining != 24*time.Second {
		t.Fatalf("Create() error = %v", err)
	}
	if errors.Is(err, ErrQuotaExceeded) {
		t.Fatal("Create() returned quota exhaustion")
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 1 {
		t.Fatalf("running = %d", running)
	}
}

func TestCreateUnavailableInsertsNothing(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{allow: func(uuid.UUID, int32) (ratelimit.Decision, error) {
		return ratelimit.Decision{}, ratelimit.ErrUnavailable
	}}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Agent", 10, 20)

	_, err := svc.Create(ctx, owner.ID, project.ID)
	if !errors.Is(err, ratelimit.ErrUnavailable) {
		t.Fatalf("Create() error = %v", err)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running = %d", running)
	}
}

func TestCreateQuotaExhausted(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	tests := []struct {
		name    string
		quota   int32
		running int64
	}{
		{name: "at quota", quota: 10, running: 10},
		{name: "above quota", quota: 10, running: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project := insertProject(t, ctx, queries, owner.ID, tt.name, tt.quota, 20)
			seedRunning(t, ctx, queries, project.ID, tt.running)
			_, err := svc.Create(ctx, owner.ID, project.ID)
			if !errors.Is(err, ErrQuotaExceeded) {
				t.Fatalf("Create() error = %v", err)
			}
			if running := countRunning(t, ctx, queries, project.ID); running != tt.running {
				t.Fatalf("running = %d", running)
			}
		})
	}
}

func TestCreateStoppedDoesNotConsumeQuota(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, &fakeAdmitter{}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "History", 1, 20)
	stopped, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	affected, err := queries.StopRunningSandbox(ctx, sqldb.StopRunningSandboxParams{
		ID:        stopped.ID,
		ProjectID: project.ID,
	})
	if err != nil || affected != 1 {
		t.Fatalf("StopRunningSandbox() = %d, %v", affected, err)
	}

	created, err := svc.Create(ctx, owner.ID, project.ID)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ID == stopped.ID || created.Status != "running" || created.StoppedAt != nil {
		t.Fatalf("sandbox = %+v", created)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 1 {
		t.Fatalf("running = %d", running)
	}
}

func TestCreateConcurrentFinalSlot(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, &fakeAdmitter{}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	guest := insertUser(t, ctx, queries, "guest")
	project := insertProject(t, ctx, queries, owner.ID, "Shared", 10, 20)
	if _, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: project.ID,
		UserID:    guest.ID,
	}); err != nil {
		t.Fatalf("AddProjectMember() error = %v", err)
	}
	seedRunning(t, ctx, queries, project.ID, 9)

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	quotaErrs := 0
	var other error
	for _, userID := range []uuid.UUID{owner.ID, guest.ID} {
		wg.Add(1)
		go func(userID uuid.UUID) {
			defer wg.Done()
			<-start
			_, err := svc.Create(ctx, userID, project.ID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrQuotaExceeded):
				quotaErrs++
			case other == nil:
				other = err
			}
		}(userID)
	}
	close(start)
	wg.Wait()

	if other != nil {
		t.Fatalf("Create() error = %v", other)
	}
	if successes != 1 || quotaErrs != 1 {
		t.Fatalf("successes = %d, quota errors = %d", successes, quotaErrs)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 10 {
		t.Fatalf("running = %d", running)
	}
}

func TestCreateIndependentProjectLocks(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, &fakeAdmitter{}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	held := insertProject(t, ctx, queries, owner.ID, "Held", 10, 20)
	other := insertProject(t, ctx, queries, owner.ID, "Other", 10, 20)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
	})
	if _, err := queries.WithTx(tx).LockAuthorizedProject(ctx, sqldb.LockAuthorizedProjectParams{
		ProjectID: held.ID,
		UserID:    owner.ID,
	}); err != nil {
		t.Fatalf("LockAuthorizedProject() error = %v", err)
	}

	createCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	created, err := svc.Create(createCtx, owner.ID, other.ID)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.ProjectID != other.ID || created.Status != "running" {
		t.Fatalf("sandbox = %+v", created)
	}
	if running := countRunning(t, ctx, queries, held.ID); running != 0 {
		t.Fatalf("held running = %d", running)
	}
}

func TestCreateCancelsWhileProjectLocked(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Locked", 10, 20)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
	})
	if _, err := queries.WithTx(tx).LockAuthorizedProject(ctx, sqldb.LockAuthorizedProjectParams{
		ProjectID: project.ID,
		UserID:    owner.ID,
	}); err != nil {
		t.Fatalf("LockAuthorizedProject() error = %v", err)
	}

	parent, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	createCtx, cancel := context.WithCancel(parent)
	errCh := make(chan error, 1)
	go func() {
		_, err := svc.Create(createCtx, owner.ID, project.ID)
		errCh <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for len(admit.snapshot()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls := admit.snapshot(); len(calls) != 1 {
		t.Fatalf("admission = %+v", calls)
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Create() error = %v", err)
		}
	case <-parent.Done():
		t.Fatal("Create() did not return")
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running = %d", running)
	}
}

func TestListProjectSandboxes(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{allow: denyAfter(24 * time.Second)}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "list-owner")
	guest := insertUser(t, ctx, queries, "list-guest")
	stranger := insertUser(t, ctx, queries, "list-stranger")
	otherOwner := insertUser(t, ctx, queries, "list-other")
	project := insertProject(t, ctx, queries, owner.ID, "Agent", 10, 20)
	other := insertProject(t, ctx, queries, otherOwner.ID, "Other", 10, 20)

	empty, err := svc.List(ctx, owner.ID, project.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() empty error = %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty sandboxes = %#v", empty)
	}

	firstTie, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() first error = %v", err)
	}
	secondTie, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() second error = %v", err)
	}
	stoppedRow, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() stopped error = %v", err)
	}
	affected, err := queries.StopRunningSandbox(ctx, sqldb.StopRunningSandboxParams{
		ID:        stoppedRow.ID,
		ProjectID: project.ID,
	})
	if err != nil {
		t.Fatalf("StopRunningSandbox() error = %v", err)
	}
	if affected != 1 {
		t.Fatalf("stopped rows = %d", affected)
	}
	foreign, err := queries.CreateSandbox(ctx, other.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() foreign error = %v", err)
	}

	shared := time.Date(2026, 11, 24, 12, 10, 0, 0, time.UTC)
	older := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	ahead := shared.Add(time.Minute)
	if err := setSandboxCreatedAt(ctx, pool, firstTie.ID, shared); err != nil {
		t.Fatal(err)
	}
	if err := setSandboxCreatedAt(ctx, pool, secondTie.ID, shared); err != nil {
		t.Fatal(err)
	}
	if err := setSandboxCreatedAt(ctx, pool, stoppedRow.ID, older); err != nil {
		t.Fatal(err)
	}
	if err := setSandboxCreatedAt(ctx, pool, foreign.ID, ahead); err != nil {
		t.Fatal(err)
	}
	leading, following := orderedSandboxIDs(firstTie.ID, secondTie.ID)

	page, err := svc.List(ctx, owner.ID, project.ID, Page{Limit: 1})
	if err != nil {
		t.Fatalf("List() first page error = %v", err)
	}
	if len(page) != 1 || page[0].ID != leading || page[0].ProjectID != project.ID || page[0].Status != "running" || page[0].StoppedAt != nil {
		t.Fatalf("first page = %+v", page)
	}

	rest, err := svc.List(ctx, owner.ID, project.ID, Page{
		Limit: 10,
		After: &Position{CreatedAt: page[0].CreatedAt, ID: page[0].ID},
	})
	if err != nil {
		t.Fatalf("List() second page error = %v", err)
	}
	if len(rest) != 2 || rest[0].ID != following || rest[0].Status != "running" || rest[0].StoppedAt != nil {
		t.Fatalf("second page head = %+v", rest)
	}
	if rest[1].ID != stoppedRow.ID || rest[1].Status != "stopped" || rest[1].StoppedAt == nil || rest[1].StoppedAt.Before(rest[1].CreatedAt) {
		t.Fatalf("stopped sandbox = %+v", rest[1])
	}
	for _, row := range append(page, rest...) {
		if row.ID == foreign.ID || row.ProjectID != project.ID {
			t.Fatalf("foreign sandbox listed: %+v", row)
		}
	}

	otherPage, err := svc.List(ctx, otherOwner.ID, other.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() other error = %v", err)
	}
	if len(otherPage) != 1 || otherPage[0].ID != foreign.ID {
		t.Fatalf("other page = %+v", otherPage)
	}

	if _, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: project.ID,
		UserID:    guest.ID,
	}); err != nil {
		t.Fatalf("AddProjectMember() error = %v", err)
	}
	guestPage, err := svc.List(ctx, guest.ID, project.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() guest error = %v", err)
	}
	if len(guestPage) != 3 || guestPage[0].ID != leading || guestPage[1].ID != following || guestPage[2].ID != stoppedRow.ID {
		t.Fatalf("guest page = %+v", guestPage)
	}

	if _, err := svc.List(ctx, stranger.ID, project.ID, Page{Limit: 50}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger error = %v", err)
	}
	if _, err := svc.List(ctx, owner.ID, uuid.New(), Page{Limit: 50}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project error = %v", err)
	}
	if calls := admit.snapshot(); len(calls) != 0 {
		t.Fatalf("admission = %+v", calls)
	}
}

func TestListSkipsUnavailableAdmission(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{allow: func(uuid.UUID, int32) (ratelimit.Decision, error) {
		return ratelimit.Decision{}, ratelimit.ErrUnavailable
	}}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "list-unavailable")
	project := insertProject(t, ctx, queries, owner.ID, "Agent", 10, 20)
	created, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	page, err := svc.List(ctx, owner.ID, project.ID, Page{Limit: 50})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page) != 1 || page[0].ID != created.ID || page[0].Status != "running" {
		t.Fatalf("page = %+v", page)
	}
	if calls := admit.snapshot(); len(calls) != 0 {
		t.Fatalf("admission = %+v", calls)
	}
}

func TestStopRunningSandbox(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{allow: denyAfter(24 * time.Second)}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Agent", 10, 20)
	created, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if err := svc.Stop(ctx, owner.ID, created.ID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	stopped, err := queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: created.ID,
		UserID:    owner.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedSandbox() error = %v", err)
	}
	if stopped.Status != "stopped" || stopped.StoppedAt == nil || stopped.StoppedAt.Before(stopped.CreatedAt) {
		t.Fatalf("stopped sandbox = %+v", stopped)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running = %d", running)
	}

	if err := svc.Stop(ctx, owner.ID, created.ID); err != nil {
		t.Fatalf("Stop() repeat error = %v", err)
	}
	repeated, err := queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: created.ID,
		UserID:    owner.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedSandbox() repeat error = %v", err)
	}
	if repeated.StoppedAt == nil || !repeated.StoppedAt.Equal(*stopped.StoppedAt) || repeated.Status != "stopped" {
		t.Fatalf("repeated sandbox = %+v", repeated)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running after repeat = %d", running)
	}
	if calls := admit.snapshot(); len(calls) != 0 {
		t.Fatalf("admission = %+v", calls)
	}
}

func TestStopAuthorization(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, &fakeAdmitter{}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	guest := insertUser(t, ctx, queries, "guest")
	stranger := insertUser(t, ctx, queries, "stranger")
	project := insertProject(t, ctx, queries, owner.ID, "Agent", 10, 20)
	created, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	if _, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: project.ID,
		UserID:    guest.ID,
	}); err != nil {
		t.Fatalf("AddProjectMember() error = %v", err)
	}

	if err := svc.Stop(ctx, stranger.ID, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stranger error = %v", err)
	}
	if err := svc.Stop(ctx, owner.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing sandbox error = %v", err)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 1 {
		t.Fatalf("running = %d", running)
	}

	if err := svc.Stop(ctx, guest.ID, created.ID); err != nil {
		t.Fatalf("Stop() guest error = %v", err)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running after guest = %d", running)
	}
}

func TestStopReleasesCapacity(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	admit := &fakeAdmitter{}
	svc := New(pool, queries, admit, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Full", 1, 20)
	created, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	if _, err := svc.Create(ctx, owner.ID, project.ID); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("Create() error = %v", err)
	}
	if err := svc.Stop(ctx, owner.ID, created.ID); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running = %d", running)
	}
	replacement, err := svc.Create(ctx, owner.ID, project.ID)
	if err != nil {
		t.Fatalf("Create() after stop error = %v", err)
	}
	if replacement.ID == created.ID || replacement.Status != "running" || replacement.StoppedAt != nil {
		t.Fatalf("sandbox = %+v", replacement)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 1 {
		t.Fatalf("running after create = %d", running)
	}
	calls := admit.snapshot()
	if len(calls) != 2 || calls[0].projectID != project.ID || calls[1].projectID != project.ID {
		t.Fatalf("admission = %+v", calls)
	}
}

func TestStopSkipsAdmission(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)

	owner := insertUser(t, ctx, queries, "owner")
	tests := []struct {
		name  string
		allow func(uuid.UUID, int32) (ratelimit.Decision, error)
	}{
		{name: "denied", allow: denyAfter(24 * time.Second)},
		{name: "unavailable", allow: func(uuid.UUID, int32) (ratelimit.Decision, error) {
			return ratelimit.Decision{}, ratelimit.ErrUnavailable
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			admit := &fakeAdmitter{allow: tt.allow}
			svc := New(pool, queries, admit, 5*time.Second)
			project := insertProject(t, ctx, queries, owner.ID, tt.name, 1, 20)
			created, err := queries.CreateSandbox(ctx, project.ID)
			if err != nil {
				t.Fatalf("CreateSandbox() error = %v", err)
			}
			if err := svc.Stop(ctx, owner.ID, created.ID); err != nil {
				t.Fatalf("Stop() error = %v", err)
			}
			if running := countRunning(t, ctx, queries, project.ID); running != 0 {
				t.Fatalf("running = %d", running)
			}
			if calls := admit.snapshot(); len(calls) != 0 {
				t.Fatalf("admission = %+v", calls)
			}
		})
	}
}

func TestStopConcurrentSameSandbox(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, &fakeAdmitter{}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	guest := insertUser(t, ctx, queries, "guest")
	project := insertProject(t, ctx, queries, owner.ID, "Shared", 10, 20)
	if _, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: project.ID,
		UserID:    guest.ID,
	}); err != nil {
		t.Fatalf("AddProjectMember() error = %v", err)
	}
	created, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var stopErr error
	for _, userID := range []uuid.UUID{owner.ID, guest.ID} {
		wg.Add(1)
		go func(userID uuid.UUID) {
			defer wg.Done()
			<-start
			err := svc.Stop(ctx, userID, created.ID)
			if err == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if stopErr == nil {
				stopErr = err
			}
		}(userID)
	}
	close(start)
	wg.Wait()
	if stopErr != nil {
		t.Fatalf("Stop() error = %v", stopErr)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running = %d", running)
	}
	stopped, err := queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: created.ID,
		UserID:    owner.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedSandbox() error = %v", err)
	}
	if stopped.Status != "stopped" || stopped.StoppedAt == nil || stopped.StoppedAt.Before(stopped.CreatedAt) {
		t.Fatalf("stopped sandbox = %+v", stopped)
	}
}

func TestStopConcurrentDistinctSandboxes(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, &fakeAdmitter{}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Pair", 10, 20)
	first, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() first error = %v", err)
	}
	second, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() second error = %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var stopErr error
	for _, sandboxID := range []uuid.UUID{first.ID, second.ID} {
		wg.Add(1)
		go func(sandboxID uuid.UUID) {
			defer wg.Done()
			<-start
			err := svc.Stop(ctx, owner.ID, sandboxID)
			if err == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if stopErr == nil {
				stopErr = err
			}
		}(sandboxID)
	}
	close(start)
	wg.Wait()
	if stopErr != nil {
		t.Fatalf("Stop() error = %v", stopErr)
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 0 {
		t.Fatalf("running = %d", running)
	}
}

func TestStopCancelsWhileProjectLocked(t *testing.T) {
	ctx := context.Background()
	pool := newSandboxDB(t)
	queries := sqldb.New(pool)
	svc := New(pool, queries, &fakeAdmitter{}, 5*time.Second)

	owner := insertUser(t, ctx, queries, "owner")
	project := insertProject(t, ctx, queries, owner.ID, "Locked", 10, 20)
	created, err := queries.CreateSandbox(ctx, project.ID)
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
	})
	if _, err := queries.WithTx(tx).LockAuthorizedProject(ctx, sqldb.LockAuthorizedProjectParams{
		ProjectID: project.ID,
		UserID:    owner.ID,
	}); err != nil {
		t.Fatalf("LockAuthorizedProject() error = %v", err)
	}

	parent, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	stopCtx, cancel := context.WithCancel(parent)
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Stop(stopCtx, owner.ID, created.ID)
	}()
	if !waitForLock(t, ctx, pool) {
		t.Fatal("Stop() did not wait for the project lock")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop() error = %v", err)
		}
	case <-parent.Done():
		t.Fatal("Stop() did not return")
	}
	if running := countRunning(t, ctx, queries, project.ID); running != 1 {
		t.Fatalf("running = %d", running)
	}
	visible, err := queries.GetAuthorizedSandbox(ctx, sqldb.GetAuthorizedSandboxParams{
		SandboxID: created.ID,
		UserID:    owner.ID,
	})
	if err != nil {
		t.Fatalf("GetAuthorizedSandbox() error = %v", err)
	}
	if visible.Status != "running" || visible.StoppedAt != nil {
		t.Fatalf("sandbox = %+v", visible)
	}
}

type admitCall struct {
	projectID uuid.UUID
	limit     int32
}

type fakeAdmitter struct {
	mu    sync.Mutex
	calls []admitCall
	allow func(uuid.UUID, int32) (ratelimit.Decision, error)
}

func (f *fakeAdmitter) Allow(_ context.Context, projectID uuid.UUID, limit int32) (ratelimit.Decision, error) {
	f.mu.Lock()
	f.calls = append(f.calls, admitCall{projectID: projectID, limit: limit})
	allow := f.allow
	f.mu.Unlock()
	if allow == nil {
		return ratelimit.Decision{Allowed: true, Remaining: time.Minute}, nil
	}
	return allow(projectID, limit)
}

func (f *fakeAdmitter) snapshot() []admitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]admitCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func denyAfter(remaining time.Duration) func(uuid.UUID, int32) (ratelimit.Decision, error) {
	return func(uuid.UUID, int32) (ratelimit.Decision, error) {
		return ratelimit.Decision{Allowed: false, Remaining: remaining}, nil
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

func insertProject(t *testing.T, ctx context.Context, queries *sqldb.Queries, userID uuid.UUID, name string, quota, rate int32) sqldb.Project {
	t.Helper()
	project, err := queries.CreateProject(ctx, sqldb.CreateProjectParams{
		Name:                name,
		MaxRunningSandboxes: quota,
		CreationRateLimit:   rate,
	})
	if err != nil {
		t.Fatalf("CreateProject() error = %v", err)
	}
	if _, err := queries.AddProjectMember(ctx, sqldb.AddProjectMemberParams{
		ProjectID: project.ID,
		UserID:    userID,
	}); err != nil {
		t.Fatalf("AddProjectMember() error = %v", err)
	}
	return project
}

func orderedSandboxIDs(left, right uuid.UUID) (leading, following uuid.UUID) {
	if bytes.Compare(left[:], right[:]) >= 0 {
		return left, right
	}
	return right, left
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

func seedRunning(t *testing.T, ctx context.Context, queries *sqldb.Queries, projectID uuid.UUID, n int64) {
	t.Helper()
	var i int64
	for i = 0; i < n; i++ {
		if _, err := queries.CreateSandbox(ctx, projectID); err != nil {
			t.Fatalf("CreateSandbox() error = %v", err)
		}
	}
}

func waitForLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		err := pool.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
		`).Scan(&waiting)
		if err != nil {
			t.Fatalf("lock wait: %v", err)
		}
		if waiting > 0 {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func countRunning(t *testing.T, ctx context.Context, queries *sqldb.Queries, projectID uuid.UUID) int64 {
	t.Helper()
	running, err := queries.CountRunningSandboxes(ctx, projectID)
	if err != nil {
		t.Fatalf("CountRunningSandboxes() error = %v", err)
	}
	return running
}

func newSandboxDB(t *testing.T) *pgxpool.Pool {
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
