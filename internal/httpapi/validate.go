package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// DefaultPageLimit is the page size used when limit is omitted.
	DefaultPageLimit = 50
	// MinPageLimit is the smallest accepted page size.
	MinPageLimit = 1
	// MaxPageLimit is the largest accepted page size.
	MaxPageLimit = 100
	// MaxCursorBytes is the largest accepted cursor string.
	MaxCursorBytes = 512
)

// Page is a validated limit and cursor.
type Page struct {
	Limit  int
	Cursor string
}

// NormalizeProjectName trims surrounding space and checks the Unicode length.
func NormalizeProjectName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if !utf8.ValidString(trimmed) {
		return "", InvalidRequest()
	}
	count := utf8.RuneCountInString(trimmed)
	if count < 1 || count > MaxProjectNameRunes {
		return "", InvalidRequest()
	}
	return trimmed, nil
}

// ParsePage validates limit and cursor. Other query fields are ignored.
func ParsePage(values url.Values) (Page, error) {
	page := Page{Limit: DefaultPageLimit}
	if limits, present := values["limit"]; present {
		if len(limits) != 1 || !canonicalLimit(limits[0]) {
			return Page{}, InvalidRequest()
		}
		number, err := strconv.Atoi(limits[0])
		if err != nil || number < MinPageLimit || number > MaxPageLimit {
			return Page{}, InvalidRequest()
		}
		page.Limit = number
	}
	if cursors, present := values["cursor"]; present {
		if len(cursors) != 1 || cursors[0] == "" || len(cursors[0]) > MaxCursorBytes {
			return Page{}, InvalidRequest()
		}
		page.Cursor = cursors[0]
	}
	return page, nil
}

// PathID reads a hyphenated UUID from a matched path value.
func PathID(r *http.Request, name string) (ID, error) {
	return ParseID(r.PathValue(name))
}

func canonicalLimit(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return len(value) == 1 || value[0] != '0'
}
