package httpapi

import (
	"errors"
	"strings"
)

const schemeBearer = "Bearer"

var errCredential = errors.New("rejected credential")

// bearerToken returns the single Bearer credential from Authorization.
// Absent, empty, repeated, and non-Bearer credentials are rejected.
func bearerToken(values []string) (string, error) {
	if len(values) != 1 {
		return "", errCredential
	}
	scheme, token, ok := splitCredential(values[0])
	if !ok || !strings.EqualFold(scheme, schemeBearer) || !token68(token) {
		return "", errCredential
	}
	return token, nil
}

func splitCredential(field string) (scheme, token string, ok bool) {
	if field == "" || field[0] == ' ' {
		return "", "", false
	}
	schemeEnd := strings.IndexByte(field, ' ')
	if schemeEnd <= 0 {
		return "", "", false
	}
	scheme = field[:schemeEnd]
	if !schemeToken(scheme) {
		return "", "", false
	}
	rest := strings.TrimLeft(field[schemeEnd:], " ")
	if rest == "" {
		return "", "", false
	}
	tokenEnd := strings.IndexByte(rest, ' ')
	if tokenEnd < 0 {
		return scheme, rest, true
	}
	token = rest[:tokenEnd]
	for i := tokenEnd; i < len(rest); i++ {
		if rest[i] != ' ' {
			return "", "", false
		}
	}
	if token == "" {
		return "", "", false
	}
	return scheme, token, true
}

func schemeToken(scheme string) bool {
	if scheme == "" {
		return false
	}
	for i := 0; i < len(scheme); i++ {
		if !isTChar(scheme[i]) {
			return false
		}
	}
	return true
}

func isTChar(c byte) bool {
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	}
}

func token68(value string) bool {
	if value == "" {
		return false
	}
	seen := false
	padding := false
	for i := 0; i < len(value); i++ {
		if value[i] == '=' {
			if !seen {
				return false
			}
			padding = true
			continue
		}
		if padding || !isToken68Char(value[i]) {
			return false
		}
		seen = true
	}
	return seen
}

func isToken68Char(c byte) bool {
	switch c {
	case '-', '.', '_', '~', '+', '/':
		return true
	default:
		return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	}
}
