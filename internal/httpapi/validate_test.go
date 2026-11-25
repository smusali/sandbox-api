package httpapi

import (
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeProjectName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "trimmed", input: " \tAgent Development\n", want: "Agent Development"},
		{name: "nbsp", input: "\u00a0Agent\u00a0", want: "Agent"},
		{name: "internal space", input: "Agent  Development", want: "Agent  Development"},
		{name: "internal newline", input: "A\nB", want: "A\nB"},
		{name: "max runes", input: strings.Repeat("你", MaxProjectNameRunes), want: strings.Repeat("你", MaxProjectNameRunes)},
		{name: "empty", input: "", wantErr: true},
		{name: "spaces", input: " \t\n", wantErr: true},
		{name: "too long", input: strings.Repeat("a", MaxProjectNameRunes+1), wantErr: true},
		{name: "too many runes", input: strings.Repeat("你", MaxProjectNameRunes+1), wantErr: true},
		{name: "invalid utf-8", input: string([]byte{0xff, 0xfe}), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeProjectName(tt.input)
			if tt.wantErr {
				assertDecodeError(t, err, CodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("name = %q", got)
			}
			if utf8.RuneCountInString(got) > MaxProjectNameRunes {
				t.Fatalf("runes = %d", utf8.RuneCountInString(got))
			}
		})
	}
}

func TestParseID(t *testing.T) {
	valid := "6D502949-7E8C-4F47-8B0C-3F1E3B913001"
	id, err := ParseID(valid)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if id.String() != "6d502949-7e8c-4f47-8b0c-3f1e3b913001" {
		t.Fatalf("canonical = %s", id.String())
	}

	rejected := []string{
		"",
		"6d502949-7e8c-4f47-8b0c-3f1e3b91300",
		"{6d502949-7e8c-4f47-8b0c-3f1e3b913001}",
		"urn:uuid:6d502949-7e8c-4f47-8b0c-3f1e3b913001",
		"6d5029497e8c4f478b0c3f1e3b913001",
		"6d502949-7e8c-4f47-8b0c-3f1e3b913001 ",
		"gggggggg-7e8c-4f47-8b0c-3f1e3b913001",
	}
	for _, value := range rejected {
		t.Run(value, func(t *testing.T) {
			_, err := ParseID(value)
			assertDecodeError(t, err, CodeInvalidRequest)
		})
	}
}

func TestParsePage(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantCursor string
		wantErr    bool
	}{
		{name: "defaults", query: "", wantLimit: DefaultPageLimit},
		{name: "ignored field", query: "limit=10&sort=name", wantLimit: 10},
		{name: "minimum", query: "limit=1", wantLimit: 1},
		{name: "maximum", query: "limit=100", wantLimit: 100},
		{name: "cursor", query: "cursor=abc", wantLimit: DefaultPageLimit, wantCursor: "abc"},
		{name: "max cursor", query: "cursor=" + strings.Repeat("a", MaxCursorBytes), wantLimit: DefaultPageLimit, wantCursor: strings.Repeat("a", MaxCursorBytes)},
		{name: "zero", query: "limit=0", wantErr: true},
		{name: "negative", query: "limit=-1", wantErr: true},
		{name: "too large", query: "limit=101", wantErr: true},
		{name: "fraction", query: "limit=1.5", wantErr: true},
		{name: "exponent", query: "limit=1e2", wantErr: true},
		{name: "plus", query: "limit=%2B50", wantErr: true},
		{name: "leading zero", query: "limit=01", wantErr: true},
		{name: "blank", query: "limit=", wantErr: true},
		{name: "word", query: "limit=abc", wantErr: true},
		{name: "space", query: "limit=+50", wantErr: true},
		{name: "duplicate limit", query: "limit=10&limit=20", wantErr: true},
		{name: "duplicate cursor", query: "cursor=a&cursor=b", wantErr: true},
		{name: "empty cursor", query: "cursor=", wantErr: true},
		{name: "long cursor", query: "cursor=" + strings.Repeat("a", MaxCursorBytes+1), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			page, err := ParsePage(values)
			if tt.wantErr {
				assertDecodeError(t, err, CodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if page.Limit != tt.wantLimit || page.Cursor != tt.wantCursor {
				t.Fatalf("page = %#v", page)
			}
		})
	}
}
