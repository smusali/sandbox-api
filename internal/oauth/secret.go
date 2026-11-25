package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

const (
	secretBytes = 32
	secretChars = 43

	maxAuthorizationCode = 512
)

func randomSecret() (string, error) {
	buf := make([]byte, secretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func bindingHash(binding string) string {
	sum := sha256.Sum256([]byte(binding))
	return hex.EncodeToString(sum[:])
}

func validSecret(value string) bool {
	if len(value) != secretChars {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !secretChar(value[i]) {
			return false
		}
	}
	return true
}

func secretChar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z':
		return true
	case c >= 'a' && c <= 'z':
		return true
	case c >= '0' && c <= '9':
		return true
	case c == '-' || c == '_':
		return true
	default:
		return false
	}
}

func validCode(code string) bool {
	if code == "" || len(code) > maxAuthorizationCode {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 0x20 || code[i] == 0x7f {
			return false
		}
	}
	return true
}
