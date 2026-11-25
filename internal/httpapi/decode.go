package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

// MaxRequestBodyBytes is the largest accepted request body.
const MaxRequestBodyBytes = 16 * 1024

// DecodeCreateProject reads a project creation body and returns the normalized name.
func DecodeCreateProject(w http.ResponseWriter, r *http.Request) (CreateProjectRequest, error) {
	body, err := prepareObject(w, r)
	if err != nil {
		return CreateProjectRequest{}, err
	}
	dec := newDecoder(body)
	var req CreateProjectRequest
	if err := finishDecode(dec, dec.Decode(&req)); err != nil {
		return CreateProjectRequest{}, err
	}
	name, err := NormalizeProjectName(req.Name)
	if err != nil {
		return CreateProjectRequest{}, err
	}
	req.Name = name
	return req, nil
}

// DecodeAddMember reads a membership body. The user id is required.
func DecodeAddMember(w http.ResponseWriter, r *http.Request) (AddMemberRequest, error) {
	body, err := prepareObject(w, r)
	if err != nil {
		return AddMemberRequest{}, err
	}
	dec := newDecoder(body)
	var req AddMemberRequest
	if err := finishDecode(dec, dec.Decode(&req)); err != nil {
		return AddMemberRequest{}, err
	}
	if req.UserID == nil {
		return AddMemberRequest{}, InvalidRequest()
	}
	return req, nil
}

// DecodeCreateSandbox accepts an empty body or one empty JSON object.
func DecodeCreateSandbox(w http.ResponseWriter, r *http.Request) error {
	body, err := readBody(w, r)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	if err := requireJSONContent(r); err != nil {
		return err
	}
	if err := requireJSONObject(body); err != nil {
		return err
	}
	dec := newDecoder(body)
	var req CreateSandboxRequest
	return finishDecode(dec, dec.Decode(&req))
}

// RejectBody reports a non-empty body on an operation that does not accept one.
func RejectBody(w http.ResponseWriter, r *http.Request) error {
	body, err := readBody(w, r)
	if err != nil {
		return err
	}
	if len(body) > 0 {
		return InvalidRequest()
	}
	return nil
}

func prepareObject(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := readBody(w, r)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, InvalidRequest()
	}
	if err := requireJSONContent(r); err != nil {
		return nil, err
	}
	if err := requireJSONObject(body); err != nil {
		return nil, err
	}
	return body, nil
}

func requireJSONObject(body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return InvalidRequest()
	}
	return nil
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			return nil, RequestTooLarge()
		}
		return nil, InvalidRequest()
	}
	return body, nil
}

func requireJSONContent(r *http.Request) error {
	header := r.Header.Get("Content-Type")
	if header == "" {
		return UnsupportedMediaType()
	}
	mediaType, params, err := mime.ParseMediaType(header)
	if err != nil || mediaType != "application/json" {
		return UnsupportedMediaType()
	}
	if len(params) == 0 {
		return nil
	}
	if len(params) == 1 && strings.EqualFold(params["charset"], "utf-8") {
		return nil
	}
	return UnsupportedMediaType()
}

func newDecoder(body []byte) *json.Decoder {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec
}

func finishDecode(dec *json.Decoder, err error) error {
	if err != nil {
		return InvalidRequest()
	}
	var extra struct{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return InvalidRequest()
	}
	return nil
}
