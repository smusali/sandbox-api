package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

const cursorFormatVersion cursorVersion = 1

// cursorEncoding is unpadded URL-safe base64 that rejects non-canonical input.
var cursorEncoding = base64.RawURLEncoding.Strict()

// errIncompleteCursor reports a next-page position that cannot be encoded.
var errIncompleteCursor = errors.New("pagination cursor is missing a position")

// Collection identifies which list a cursor belongs to.
type Collection string

const (
	// CollectionProjects is the accessible-project list.
	// Its scope is the authenticated application user.
	CollectionProjects Collection = "projects"
	// CollectionSandboxes is the project sandbox list.
	// Its scope is the requested project.
	CollectionSandboxes Collection = "sandboxes"
)

func (c Collection) known() bool {
	return c == CollectionProjects || c == CollectionSandboxes
}

// cursorVersion accepts only the canonical JSON integer 1.
type cursorVersion int

func (v *cursorVersion) UnmarshalJSON(data []byte) error {
	if string(data) != "1" {
		return InvalidRequest()
	}
	*v = cursorFormatVersion
	return nil
}

// cursorDocument is the decoded pagination position.
type cursorDocument struct {
	Version    cursorVersion `json:"version"`
	Collection Collection    `json:"collection"`
	Scope      ID            `json:"scope"`
	CreatedAt  Time          `json:"created_at"`
	ResourceID ID            `json:"id"`
}

// Position is the creation time and id of one returned record.
type Position struct {
	createdAt time.Time
	id        ID
}

// CreatedAt is the record creation timestamp in UTC.
func (p Position) CreatedAt() time.Time {
	return p.createdAt
}

// ID is the record UUID.
func (p Position) ID() uuid.UUID {
	return p.id.UUID()
}

// Span is the row window for one page.
type Span struct {
	limit    int32
	after    Position
	hasAfter bool
}

// Limit is the number of rows to read, one past the page size.
func (s Span) Limit() int32 {
	return s.limit
}

// HasAfter reports whether the read continues after a cursor position.
func (s Span) HasAfter() bool {
	return s.hasAfter
}

// After is the position a subsequent-page query compares against.
// It is set only when HasAfter is true.
func (s Span) After() Position {
	return s.after
}

// Open selects the database window for one collection page.
// An empty cursor is the first page. A cursor continues after its position.
// Scope is the authenticated user for projects and the requested project for sandboxes.
// A cursor records a page position. Callers authorize the collection on every request.
func Open(page Page, collection Collection, scope ID) (Span, error) {
	limit, err := pageRowLimit(page.Limit)
	if err != nil {
		return Span{}, err
	}
	if len(page.Cursor) > MaxCursorBytes {
		return Span{}, InvalidRequest()
	}
	if page.Cursor == "" {
		return Span{limit: limit}, nil
	}
	after, err := decodeCursor(page.Cursor, collection, scope)
	if err != nil {
		return Span{}, err
	}
	return Span{limit: limit, after: after, hasAfter: true}, nil
}

// pageRowLimit is the SQL row count for one page, including the extra row
// used to decide whether another page exists.
func pageRowLimit(pageSize int) (int32, error) {
	if pageSize < MinPageLimit || pageSize > MaxPageLimit {
		return 0, InvalidRequest()
	}
	return int32(pageSize + 1), nil
}

// Trim keeps at most limit records from a read of limit plus one.
// more reports that another record was present past the page.
func Trim(fetched, limit int) (int, bool) {
	if fetched < 0 {
		fetched = 0
	}
	if limit < MinPageLimit {
		return 0, false
	}
	if fetched > limit {
		return limit, true
	}
	return fetched, false
}

// NextCursor encodes the last returned record when another page exists.
// A false more value returns a nil cursor, including an empty page.
func NextCursor(collection Collection, scope ID, created time.Time, id ID, more bool) (*string, error) {
	if !more {
		return nil, nil
	}
	if !collection.known() || scope.UUID() == uuid.Nil || created.IsZero() || id.UUID() == uuid.Nil {
		return nil, errIncompleteCursor
	}
	encoded, err := encodeCursor(collection, scope, created, id)
	if err != nil {
		return nil, err
	}
	return &encoded, nil
}

func encodeCursor(collection Collection, scope ID, created time.Time, id ID) (string, error) {
	payload, err := json.Marshal(cursorDocument{
		Version:    cursorFormatVersion,
		Collection: collection,
		Scope:      scope,
		CreatedAt:  NewTime(created),
		ResourceID: id,
	})
	if err != nil {
		return "", err
	}
	return cursorEncoding.EncodeToString(payload), nil
}

func decodeCursor(token string, collection Collection, scope ID) (Position, error) {
	if token == "" || len(token) > MaxCursorBytes || !collection.known() {
		return Position{}, InvalidRequest()
	}
	raw, err := cursorEncoding.DecodeString(token)
	if err != nil {
		return Position{}, InvalidRequest()
	}
	if err := requireJSONObject(raw); err != nil {
		return Position{}, err
	}
	dec := newDecoder(raw)
	var doc cursorDocument
	if err := finishDecode(dec, dec.Decode(&doc)); err != nil {
		return Position{}, err
	}
	if doc.Version != cursorFormatVersion || doc.Collection != collection || doc.Scope != scope {
		return Position{}, InvalidRequest()
	}
	if doc.Scope.UUID() == uuid.Nil || doc.ResourceID.UUID() == uuid.Nil || doc.CreatedAt.Time().IsZero() {
		return Position{}, InvalidRequest()
	}
	return Position{createdAt: doc.CreatedAt.Time(), id: doc.ResourceID}, nil
}
