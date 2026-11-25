package httpapi

import (
	"encoding/json"
	"time"

	"uuid"
)

const (
	// SandboxRunning is the status of a sandbox that counts toward quota.
	SandboxRunning = "running"
	// SandboxStopped is the terminal sandbox status.
	SandboxStopped = "stopped"
	// TokenTypeBearer is the token type returned by a completed login.
	TokenTypeBearer = "Bearer"
)

// Time is a UTC timestamp serialized as RFC 3339.
type Time struct {
	instant time.Time
}

// NewTime converts t to UTC for a response timestamp.
func NewTime(t time.Time) Time {
	return Time{instant: t.UTC()}
}

// Time returns the UTC instant.
func (t Time) Time() time.Time {
	return t.instant
}

// MarshalJSON writes the timestamp in UTC, keeping significant fractional seconds.
func (t Time) MarshalJSON() ([]byte, error) {
	formatted := t.instant.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	return json.Marshal(formatted)
}

// UnmarshalJSON reads an RFC 3339 timestamp and stores it in UTC.
func (t *Time) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return InvalidRequest()
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return InvalidRequest()
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return InvalidRequest()
	}
	t.instant = parsed.UTC()
	return nil
}

// ID is a resource identifier in the public API.
type ID struct {
	value uuid.UUID
}

// ParseID accepts the hyphenated UUID form and rejects every other spelling.
func ParseID(value string) (ID, error) {
	if !hyphenatedUUID(value) {
		return ID{}, InvalidRequest()
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return ID{}, InvalidRequest()
	}
	return ID{value: parsed}, nil
}

// UUID returns the identifier for persistence calls.
func (id ID) UUID() uuid.UUID {
	return id.value
}

// String returns the lowercase hyphenated form.
func (id ID) String() string {
	return id.value.String()
}

// MarshalJSON writes the canonical UUID string.
func (id ID) MarshalJSON() ([]byte, error) {
	return json.Marshal(id.String())
}

// UnmarshalJSON accepts only a JSON string in the hyphenated UUID form.
func (id *ID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return InvalidRequest()
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return InvalidRequest()
	}
	parsed, err := ParseID(text)
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}

func hyphenatedUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return false
			}
		default:
			if !isHex(value[i]) {
				return false
			}
		}
	}
	return true
}

func isHex(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'f':
		return true
	case c >= 'A' && c <= 'F':
		return true
	default:
		return false
	}
}

// Project is the public project representation.
type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt Time   `json:"created_at"`
}

// NewProject builds a project response from persisted values.
func NewProject(id ID, name string, created time.Time) Project {
	return Project{ID: id.String(), Name: name, CreatedAt: NewTime(created)}
}

// Membership is the public membership representation.
type Membership struct {
	ProjectID string `json:"project_id"`
	UserID    string `json:"user_id"`
	CreatedAt Time   `json:"created_at"`
}

// NewMembership builds a membership response from persisted values.
func NewMembership(projectID ID, userID ID, created time.Time) Membership {
	return Membership{ProjectID: projectID.String(), UserID: userID.String(), CreatedAt: NewTime(created)}
}

// Sandbox is the public sandbox representation.
type Sandbox struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Status    string `json:"status"`
	CreatedAt Time   `json:"created_at"`
	StoppedAt *Time  `json:"stopped_at"`
}

// NewSandbox builds a sandbox response. A nil stopped time is JSON null.
func NewSandbox(id ID, projectID ID, status string, created time.Time, stopped *time.Time) Sandbox {
	var stoppedAt *Time
	if stopped != nil {
		value := NewTime(*stopped)
		stoppedAt = &value
	}
	return Sandbox{
		ID:        id.String(),
		ProjectID: projectID.String(),
		Status:    status,
		CreatedAt: NewTime(created),
		StoppedAt: stoppedAt,
	}
}

// ProjectPage is one page of projects.
type ProjectPage struct {
	Items      []Project `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// NewProjectPage returns an empty item list when items is nil.
func NewProjectPage(items []Project, next *string) ProjectPage {
	if items == nil {
		items = []Project{}
	}
	return ProjectPage{Items: items, NextCursor: next}
}

// SandboxPage is one page of sandboxes.
type SandboxPage struct {
	Items      []Sandbox `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// NewSandboxPage returns an empty item list when items is nil.
func NewSandboxPage(items []Sandbox, next *string) SandboxPage {
	if items == nil {
		items = []Sandbox{}
	}
	return SandboxPage{Items: items, NextCursor: next}
}

// TokenResponse is the completed login response.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   *int   `json:"expires_in,omitempty"`
	UserID      string `json:"user_id"`
}

// NewTokenResponse builds a login response. expiresIn is omitted when it is nil.
func NewTokenResponse(accessToken string, expiresIn *int, userID ID) TokenResponse {
	return TokenResponse{
		AccessToken: accessToken,
		TokenType:   TokenTypeBearer,
		ExpiresIn:   expiresIn,
		UserID:      userID.String(),
	}
}

// CreateProjectRequest is the body accepted when creating a project.
type CreateProjectRequest struct {
	Name string `json:"name"`
}

// AddMemberRequest is the body accepted when adding a project member.
type AddMemberRequest struct {
	UserID *ID `json:"user_id"`
}

// CreateSandboxRequest is the empty object accepted when creating a sandbox.
type CreateSandboxRequest struct{}

// MaxProjectNameRunes is the longest accepted project name, in Unicode code points.
const MaxProjectNameRunes = 255
