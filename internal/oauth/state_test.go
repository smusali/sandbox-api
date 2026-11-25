package oauth

import (
	"errors"
	"testing"
)

func TestDecodeConsume(t *testing.T) {
	tests := []struct {
		name    string
		parts   []string
		want    string
		wantErr error
	}{
		{name: "missing", parts: []string{"missing", ""}, wantErr: errStateMissing},
		{name: "mismatch", parts: []string{"mismatch", ""}, wantErr: errStateMismatch},
		{name: "corrupt", parts: []string{"corrupt", ""}, wantErr: errStateCorrupt},
		{name: "ok", parts: []string{"ok", "verifier"}, want: "verifier"},
		{name: "short", parts: []string{"ok"}, wantErr: errStateReply},
		{name: "missing with verifier", parts: []string{"missing", "verifier"}, wantErr: errStateReply},
		{name: "empty verifier", parts: []string{"ok", ""}, wantErr: errStateCorrupt},
		{name: "unknown", parts: []string{"other", ""}, wantErr: errStateReply},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeConsume(tt.parts)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("decodeConsume() = %q, %v", got, err)
			}
		})
	}
}
