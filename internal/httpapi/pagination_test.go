package httpapi

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCursorRoundTripPreservesPosition(t *testing.T) {
	userID := mustID(t, "de498973-c030-4fc2-b075-4b5dbb421002")
	projectID := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	sandboxID := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721003")
	micros := time.Date(2026, 11, 24, 12, 0, 0, 123456000, time.UTC)
	whole := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	offset := time.Date(2026, 11, 24, 4, 0, 0, 123456000, time.FixedZone("PST", -8*3600))

	tests := []struct {
		name       string
		collection Collection
		scope      ID
		id         ID
		created    time.Time
		wantJSON   string
		limit      int
		wantLimit  int32
	}{
		{
			name:       "sandbox microseconds",
			collection: CollectionSandboxes,
			scope:      projectID,
			id:         sandboxID,
			created:    micros,
			wantJSON:   `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00.123456Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`,
			limit:      DefaultPageLimit,
			wantLimit:  51,
		},
		{
			name:       "project whole second",
			collection: CollectionProjects,
			scope:      userID,
			id:         projectID,
			created:    whole,
			wantJSON:   `{"version":1,"collection":"projects","scope":"de498973-c030-4fc2-b075-4b5dbb421002","created_at":"2026-11-24T12:00:00Z","id":"6d502949-7e8c-4f47-8b0c-3f1e3b913001"}`,
			limit:      MinPageLimit,
			wantLimit:  2,
		},
		{
			name:       "offset becomes utc",
			collection: CollectionSandboxes,
			scope:      projectID,
			id:         sandboxID,
			created:    offset,
			wantJSON:   `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00.123456Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`,
			limit:      MaxPageLimit,
			wantLimit:  101,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := NextCursor(tt.collection, tt.scope, tt.created, tt.id, true)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if encoded == nil {
				t.Fatal("cursor is nil")
			}
			raw, err := cursorEncoding.DecodeString(*encoded)
			if err != nil {
				t.Fatalf("decode token: %v", err)
			}
			if string(raw) != tt.wantJSON {
				t.Fatalf("json = %s", raw)
			}

			span, err := Open(Page{Limit: tt.limit, Cursor: *encoded}, tt.collection, tt.scope)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if !span.HasAfter() || span.Limit() != tt.wantLimit {
				t.Fatalf("span limit = %d, after = %v", span.Limit(), span.HasAfter())
			}
			if !span.After().CreatedAt().Equal(tt.created.UTC()) {
				t.Fatalf("created = %s", span.After().CreatedAt())
			}
			if span.After().ID() != tt.id.UUID() {
				t.Fatalf("id = %s", span.After().ID())
			}
		})
	}
}

func TestOpenFirstPage(t *testing.T) {
	scope := mustID(t, "de498973-c030-4fc2-b075-4b5dbb421002")
	tests := []struct {
		name      string
		limit     int
		wantLimit int32
	}{
		{name: "default", limit: DefaultPageLimit, wantLimit: 51},
		{name: "minimum", limit: MinPageLimit, wantLimit: 2},
		{name: "maximum", limit: MaxPageLimit, wantLimit: 101},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			span, err := Open(Page{Limit: tt.limit}, CollectionProjects, scope)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if span.HasAfter() || span.Limit() != tt.wantLimit {
				t.Fatalf("span limit = %d, after = %v", span.Limit(), span.HasAfter())
			}
			if span.After().ID() != uuid.Nil || !span.After().CreatedAt().IsZero() {
				t.Fatal("first page has a position")
			}
		})
	}
}

func TestOpenRejectsPage(t *testing.T) {
	scope := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	for _, limit := range []int{0, -1, MaxPageLimit + 1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			_, err := Open(Page{Limit: limit}, CollectionSandboxes, scope)
			assertDecodeError(t, err, CodeInvalidRequest)
		})
	}
	_, err := Open(Page{Limit: DefaultPageLimit, Cursor: strings.Repeat("a", MaxCursorBytes+1)}, CollectionSandboxes, scope)
	assertDecodeError(t, err, CodeInvalidRequest)
}

func TestOpenRejectsMalformedCursor(t *testing.T) {
	projectID := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	sandboxID := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721003")
	created := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	validPtr, err := NextCursor(CollectionSandboxes, projectID, created, sandboxID, true)
	if err != nil || validPtr == nil {
		t.Fatalf("cursor: %v", err)
	}
	valid := *validPtr
	payload := `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`

	tests := []struct {
		name   string
		cursor string
	}{
		{name: "alphabet", cursor: "!!!!"},
		{name: "plus", cursor: "+++"},
		{name: "slash", cursor: "///"},
		{name: "padding", cursor: valid + "="},
		{name: "noncanonical", cursor: nonCanonicalCursor(t, payload)},
		{name: "array", cursor: cursorPayload(t, `[]`)},
		{name: "null", cursor: cursorPayload(t, `null`)},
		{name: "unknown field", cursor: cursorPayload(t, `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003","filter":"running"}`)},
		{name: "trailing value", cursor: cursorPayload(t, payload+`{"version":1}`)},
		{name: "version zero", cursor: cursorPayload(t, `{"version":0,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "version two", cursor: cursorPayload(t, `{"version":2,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "version fraction", cursor: cursorPayload(t, `{"version":1.0,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "version exponent", cursor: cursorPayload(t, `{"version":1e0,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "version string", cursor: cursorPayload(t, `{"version":"1","collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "missing version", cursor: cursorPayload(t, `{"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "bad timestamp", cursor: cursorPayload(t, `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"yesterday","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "missing timestamp", cursor: cursorPayload(t, `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","id":"7719ac21-1b89-4865-89b5-0ed2f8721003"}`)},
		{name: "bad uuid", cursor: cursorPayload(t, `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"not-a-uuid"}`)},
		{name: "zero uuid", cursor: cursorPayload(t, `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":"00000000-0000-0000-0000-000000000000"}`)},
		{name: "missing id", cursor: cursorPayload(t, `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z"}`)},
		{name: "null id", cursor: cursorPayload(t, `{"version":1,"collection":"sandboxes","scope":"6d502949-7e8c-4f47-8b0c-3f1e3b913001","created_at":"2026-11-24T12:00:00Z","id":null}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(Page{Limit: DefaultPageLimit, Cursor: tt.cursor}, CollectionSandboxes, projectID)
			assertDecodeError(t, err, CodeInvalidRequest)
		})
	}
}

func TestOpenRejectsCrossScopeCursor(t *testing.T) {
	userA := mustID(t, "de498973-c030-4fc2-b075-4b5dbb421002")
	userB := mustID(t, "11111111-1111-4111-8111-111111111111")
	projectA := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	projectB := mustID(t, "22222222-2222-4222-8222-222222222222")
	sandboxID := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721003")
	created := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	projectCursor, err := NextCursor(CollectionProjects, userA, created, projectA, true)
	if err != nil || projectCursor == nil {
		t.Fatalf("project cursor: %v", err)
	}
	sandboxCursor, err := NextCursor(CollectionSandboxes, projectA, created, sandboxID, true)
	if err != nil || sandboxCursor == nil {
		t.Fatalf("sandbox cursor: %v", err)
	}

	rejected := []struct {
		name       string
		cursor     string
		collection Collection
		scope      ID
	}{
		{name: "other user", cursor: *projectCursor, collection: CollectionProjects, scope: userB},
		{name: "project cursor on sandboxes", cursor: *projectCursor, collection: CollectionSandboxes, scope: projectA},
		{name: "other project", cursor: *sandboxCursor, collection: CollectionSandboxes, scope: projectB},
		{name: "sandbox cursor on projects", cursor: *sandboxCursor, collection: CollectionProjects, scope: userA},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(Page{Limit: DefaultPageLimit, Cursor: tt.cursor}, tt.collection, tt.scope)
			assertDecodeError(t, err, CodeInvalidRequest)
		})
	}
}

func TestEqualTimestampsStayDistinct(t *testing.T) {
	scope := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	created := time.Date(2026, 11, 24, 12, 0, 0, 123456000, time.UTC)
	first := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721003")
	second := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721004")

	firstCursor, err := NextCursor(CollectionSandboxes, scope, created, first, true)
	if err != nil || firstCursor == nil {
		t.Fatalf("first: %v", err)
	}
	secondCursor, err := NextCursor(CollectionSandboxes, scope, created, second, true)
	if err != nil || secondCursor == nil {
		t.Fatalf("second: %v", err)
	}
	if *firstCursor == *secondCursor {
		t.Fatal("cursors match")
	}

	firstSpan, err := Open(Page{Limit: DefaultPageLimit, Cursor: *firstCursor}, CollectionSandboxes, scope)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	secondSpan, err := Open(Page{Limit: DefaultPageLimit, Cursor: *secondCursor}, CollectionSandboxes, scope)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	if !firstSpan.After().CreatedAt().Equal(secondSpan.After().CreatedAt()) {
		t.Fatal("timestamps differ")
	}
	if firstSpan.After().ID() == secondSpan.After().ID() {
		t.Fatal("ids match")
	}
}

func TestTrimAndNextCursor(t *testing.T) {
	scope := mustID(t, "6d502949-7e8c-4f47-8b0c-3f1e3b913001")
	created := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	kept := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721003")
	extra := mustID(t, "7719ac21-1b89-4865-89b5-0ed2f8721004")

	tests := []struct {
		name    string
		fetched int
		limit   int
		keep    int
		more    bool
	}{
		{name: "empty", fetched: 0, limit: DefaultPageLimit, keep: 0, more: false},
		{name: "final page", fetched: DefaultPageLimit, limit: DefaultPageLimit, keep: DefaultPageLimit, more: false},
		{name: "full page", fetched: DefaultPageLimit + 1, limit: DefaultPageLimit, keep: DefaultPageLimit, more: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keep, more := Trim(tt.fetched, tt.limit)
			if keep != tt.keep || more != tt.more {
				t.Fatalf("trim = %d, %v", keep, more)
			}
			cursor, err := NextCursor(CollectionSandboxes, scope, created, kept, more)
			if err != nil {
				t.Fatalf("cursor: %v", err)
			}
			if !more {
				if cursor != nil {
					t.Fatal("final cursor is set")
				}
				return
			}
			if cursor == nil {
				t.Fatal("cursor is nil")
			}
			span, err := Open(Page{Limit: tt.limit, Cursor: *cursor}, CollectionSandboxes, scope)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if span.After().ID() != kept.UUID() || span.After().ID() == extra.UUID() {
				t.Fatalf("id = %s", span.After().ID())
			}
		})
	}

	empty, err := NextCursor(CollectionProjects, scope, time.Time{}, ID{}, false)
	if err != nil || empty != nil {
		t.Fatalf("empty cursor = %v, %v", empty, err)
	}
	_, err = NextCursor(CollectionSandboxes, scope, created, ID{}, true)
	var apiErr *Error
	if err == nil || errors.As(err, &apiErr) {
		t.Fatalf("error = %v", err)
	}
}

// nonCanonicalCursor encodes payload with unused base64 bits set.
// The loose decoder accepts it. Strict decoding rejects it.
func nonCanonicalCursor(t *testing.T, payload string) string {
	t.Helper()
	if len(payload)%3 == 0 {
		payload += " "
	}
	token := base64.RawURLEncoding.EncodeToString([]byte(payload))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := token[len(token)-1]
	for i := 0; i < len(alphabet); i++ {
		if alphabet[i] == last {
			continue
		}
		candidate := token[:len(token)-1] + alphabet[i:i+1]
		got, err := base64.RawURLEncoding.DecodeString(candidate)
		if err != nil || string(got) != payload {
			continue
		}
		if _, err := base64.RawURLEncoding.Strict().DecodeString(candidate); err != nil {
			return candidate
		}
	}
	t.Fatal("no non-canonical cursor")
	return ""
}

func cursorPayload(t *testing.T, payload string) string {
	t.Helper()
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}
