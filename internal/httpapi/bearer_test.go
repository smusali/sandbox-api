package httpapi

import "testing"

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{name: "absent", values: nil},
		{name: "empty list", values: []string{}},
		{name: "empty field", values: []string{""}},
		{name: "scheme only", values: []string{"Bearer"}},
		{name: "empty token", values: []string{"Bearer "}},
		{name: "blank token", values: []string{"Bearer   "}},
		{name: "unsupported scheme", values: []string{"Basic abc"}},
		{name: "repeated field", values: []string{"Bearer a", "Bearer b"}},
		{name: "comma separated", values: []string{"Bearer a, Bearer b"}},
		{name: "comma inside token", values: []string{"Bearer aaa,bbb"}},
		{name: "extra token", values: []string{"Bearer a b"}},
		{name: "leading space", values: []string{" Bearer abc"}},
		{name: "tab separator", values: []string{"Bearer\tabc"}},
		{name: "padding only", values: []string{"Bearer ="}},
		{name: "lowercase scheme", values: []string{"bearer abc"}, want: "abc"},
		{name: "uppercase scheme", values: []string{"BEARER abc"}, want: "abc"},
		{name: "extra spaces", values: []string{"Bearer  abc"}, want: "abc"},
		{name: "trailing spaces", values: []string{"Bearer abc   "}, want: "abc"},
		{name: "padding", values: []string{"Bearer abc="}, want: "abc="},
		{name: "dotted token", values: []string{"Bearer abc.def.ghi"}, want: "abc.def.ghi"},
		{name: "token characters", values: []string{"Bearer azAZ09-._~+/"}, want: "azAZ09-._~+/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bearerToken(tt.values)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("token = %q", got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("token = %q, err = %v", got, err)
			}
		})
	}
}
