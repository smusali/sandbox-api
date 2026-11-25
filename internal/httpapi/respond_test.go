package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"uuid"
)

func TestProjectResponseUsesUTCAndPreservesFraction(t *testing.T) {
	id := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	created := time.Date(2026, 11, 24, 12, 0, 0, 123456000, time.FixedZone("PST", -8*3600))

	rec := httptest.NewRecorder()
	WriteProject(rec, http.StatusCreated, NewProject(id, "A&B", created))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"name":"A&B"`) || strings.Contains(body, `\u0026`) {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, `"created_at":"2026-11-24T20:00:00.123456Z"`) {
		t.Fatalf("body = %s", body)
	}

	var got Project
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.CreatedAt.Time().Equal(created.UTC()) {
		t.Fatalf("created = %s", got.CreatedAt.Time())
	}
	if got.ID != id.String() {
		t.Fatalf("id = %s", got.ID)
	}
}

func TestWholeSecondTimestampOmitsFraction(t *testing.T) {
	id := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	created := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	rec := httptest.NewRecorder()
	WriteMembership(rec, http.StatusOK, NewMembership(id, id, created))
	if !strings.Contains(rec.Body.String(), `"created_at":"2026-11-24T20:00:00Z"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestSandboxStoppedAtNullAndValue(t *testing.T) {
	projectID := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	sandboxID := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721003")
	created := time.Date(2026, 11, 24, 20, 10, 0, 0, time.UTC)

	running := httptest.NewRecorder()
	WriteSandbox(running, http.StatusCreated, NewSandbox(sandboxID, projectID, SandboxRunning, created, nil))
	if !strings.Contains(running.Body.String(), `"stopped_at":null`) {
		t.Fatalf("body = %s", running.Body.String())
	}
	if !strings.Contains(running.Body.String(), `"status":"running"`) {
		t.Fatalf("body = %s", running.Body.String())
	}

	stoppedAt := created.Add(5 * time.Minute)
	stopped := httptest.NewRecorder()
	WriteSandbox(stopped, http.StatusOK, NewSandbox(sandboxID, projectID, SandboxStopped, created, &stoppedAt))
	if !strings.Contains(stopped.Body.String(), `"stopped_at":"2026-11-24T20:15:00Z"`) {
		t.Fatalf("body = %s", stopped.Body.String())
	}
}

func TestEmptyCollectionsAreArrays(t *testing.T) {
	projects := httptest.NewRecorder()
	WriteProjectPage(projects, http.StatusOK, NewProjectPage(nil, nil))
	assertEmptyPage(t, projects.Body.String())

	sandboxes := httptest.NewRecorder()
	WriteSandboxPage(sandboxes, http.StatusOK, NewSandboxPage(nil, nil))
	assertEmptyPage(t, sandboxes.Body.String())

	cursor := "next"
	withCursor := httptest.NewRecorder()
	WriteProjectPage(withCursor, http.StatusOK, NewProjectPage(nil, &cursor))
	if !strings.Contains(withCursor.Body.String(), `"next_cursor":"next"`) {
		t.Fatalf("body = %s", withCursor.Body.String())
	}
}

func TestTokenResponseOmitsMissingExpiry(t *testing.T) {
	userID := mustID(t, "11111111-1111-4111-8111-111111111111")
	rec := httptest.NewRecorder()
	NoStore(rec)
	WriteToken(rec, http.StatusOK, NewTokenResponse("a&b<c", nil, userID))
	body := rec.Body.String()
	if strings.Contains(body, "expires_in") {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, `"access_token":"a&b<c"`) || strings.Contains(body, `\u003c`) {
		t.Fatalf("body = %s", body)
	}
	if !strings.Contains(body, `"token_type":"Bearer"`) {
		t.Fatalf("body = %s", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}

	expires := 0
	withExpiry := httptest.NewRecorder()
	WriteToken(withExpiry, http.StatusOK, NewTokenResponse("token", &expires, userID))
	if !strings.Contains(withExpiry.Body.String(), `"expires_in":0`) {
		t.Fatalf("body = %s", withExpiry.Body.String())
	}
}

func TestWriteNoContentHasEmptyBody(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteNoContent(rec)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
}

func assertEmptyPage(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, `"items":[]`) || !strings.Contains(body, `"next_cursor":null`) {
		t.Fatalf("body = %s", body)
	}
	if strings.Contains(body, `"items":null`) {
		t.Fatalf("body = %s", body)
	}
}

func mustID(t *testing.T, value string) ID {
	t.Helper()
	id, err := ParseID(value)
	if err != nil {
		t.Fatalf("ParseID(%q) error = %v", value, err)
	}
	if id.UUID() != uuid.MustParse(value) {
		t.Fatalf("UUID = %s", id.UUID())
	}
	return id
}
