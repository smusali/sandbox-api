package httpapi

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
)

var internalErrorJSON = []byte(`{"error":{"code":"INTERNAL_ERROR","message":"The request could not be completed."}}`)

// NoStore tells caches not to store the response.
func NoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

// WriteProject writes a project response.
func WriteProject(w http.ResponseWriter, status int, project Project) {
	writeEncoded(w, status, func(enc *json.Encoder) error {
		return enc.Encode(project)
	})
}

// WriteMembership writes a membership response.
func WriteMembership(w http.ResponseWriter, status int, membership Membership) {
	writeEncoded(w, status, func(enc *json.Encoder) error {
		return enc.Encode(membership)
	})
}

// WriteSandbox writes a sandbox response.
func WriteSandbox(w http.ResponseWriter, status int, sandbox Sandbox) {
	writeEncoded(w, status, func(enc *json.Encoder) error {
		return enc.Encode(sandbox)
	})
}

// WriteProjectPage writes one page of projects.
func WriteProjectPage(w http.ResponseWriter, status int, page ProjectPage) {
	writeEncoded(w, status, func(enc *json.Encoder) error {
		return enc.Encode(page)
	})
}

// WriteSandboxPage writes one page of sandboxes.
func WriteSandboxPage(w http.ResponseWriter, status int, page SandboxPage) {
	writeEncoded(w, status, func(enc *json.Encoder) error {
		return enc.Encode(page)
	})
}

// WriteToken writes a completed login response.
func WriteToken(w http.ResponseWriter, status int, token TokenResponse) {
	writeEncoded(w, status, func(enc *json.Encoder) error {
		return enc.Encode(token)
	})
}

// WriteNoContent writes HTTP 204 with an empty body.
func WriteNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

func writeEncoded(w http.ResponseWriter, status int, encode func(*json.Encoder) error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := encode(enc); err != nil {
		log.Printf("internal error: %v", err)
		writeBody(w, http.StatusInternalServerError, internalErrorJSON)
		return
	}
	writeBody(w, status, trimNewline(buf.Bytes()))
}

func writeBody(w http.ResponseWriter, status int, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func trimNewline(payload []byte) []byte {
	if len(payload) > 0 && payload[len(payload)-1] == '\n' {
		return payload[:len(payload)-1]
	}
	return payload
}
