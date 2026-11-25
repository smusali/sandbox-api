package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLoadAcceptsFixture(t *testing.T) {
	cfg, err := load(envGetter(validEnv()))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Fatalf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.DefaultMaxRunningSandboxes != 10 || cfg.DefaultCreationRateLimit != 20 {
		t.Fatalf("limits = %d, %d", cfg.DefaultMaxRunningSandboxes, cfg.DefaultCreationRateLimit)
	}
	if cfg.RateLimitWindow != time.Minute || cfg.OAuthStateTTL != 5*time.Minute || cfg.RequestTimeout != 5*time.Second {
		t.Fatalf("durations = %s, %s, %s", cfg.RateLimitWindow, cfg.OAuthStateTTL, cfg.RequestTimeout)
	}
	if cfg.AuthorizationEndpoint() != "http://localhost:4444/oauth2/auth" {
		t.Fatalf("authorization endpoint = %q", cfg.AuthorizationEndpoint())
	}
	if cfg.TokenEndpoint() != "http://localhost:4444/oauth2/token" {
		t.Fatalf("token endpoint = %q", cfg.TokenEndpoint())
	}
	if cfg.JWKSEndpoint() != "http://localhost:4444/.well-known/jwks.json" {
		t.Fatalf("jwks endpoint = %q", cfg.JWKSEndpoint())
	}
}

func TestLoadRejectsInvalidFields(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "listen address", key: "HTTP_ADDR", value: "8080", want: "must be a host:port address"},
		{name: "listen port", key: "HTTP_ADDR", value: ":0", want: "must include a TCP port"},
		{name: "database scheme", key: "DATABASE_URL", value: "http://localhost:5432/e2b", want: "absolute postgres or postgresql URL"},
		{name: "database host", key: "DATABASE_URL", value: "postgres:///e2b", want: "absolute postgres or postgresql URL"},
		{name: "redis address", key: "REDIS_ADDR", value: "localhost", want: "must be a host:port address"},
		{name: "issuer slash", key: "OAUTH_ISSUER", value: "http://localhost:4444/", want: "must not have a trailing slash"},
		{name: "issuer query", key: "OAUTH_ISSUER", value: "http://localhost:4444?x=1", want: "without userinfo, path, query, or fragment"},
		{name: "issuer userinfo", key: "OAUTH_ISSUER", value: "http://user:pass@localhost:4444", want: "without userinfo, path, query, or fragment"},
		{name: "redirect fragment", key: "OAUTH_REDIRECT_URI", value: "http://localhost:8080/auth/callback#x", want: "without userinfo, query, or fragment"},
		{name: "redirect scheme", key: "OAUTH_REDIRECT_URI", value: "ftp://localhost:8080/auth/callback", want: "absolute http or https URL"},
		{name: "audience placeholder", key: "OAUTH_AUDIENCE", value: "<verified-api-audience>", want: "must be a concrete audience"},
		{name: "audience brackets", key: "OAUTH_AUDIENCE", value: "urn:<api>", want: "must be a concrete audience"},
		{name: "audience space", key: "OAUTH_AUDIENCE", value: "urn:e2b: sandbox", want: "must not contain whitespace"},
		{name: "audience client id", key: "OAUTH_AUDIENCE", value: "e2b-assignment", want: "must be distinct from the OAuth client id"},
		{name: "quota", key: "DEFAULT_MAX_RUNNING_SANDBOXES", value: "0", want: "must be a positive integer"},
		{name: "rate limit", key: "DEFAULT_CREATION_RATE_LIMIT", value: "-1", want: "must be a positive integer"},
		{name: "window", key: "RATE_LIMIT_WINDOW", value: "0s", want: "must be a positive duration"},
		{name: "state ttl", key: "OAUTH_STATE_TTL", value: "5", want: "must be a positive duration"},
		{name: "timeout", key: "REQUEST_TIMEOUT", value: "-5s", want: "must be a positive duration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			env[tt.key] = tt.value
			_, err := load(envGetter(env))
			var invalid *InvalidConfigError
			if !errors.As(err, &invalid) {
				t.Fatalf("error = %v", err)
			}
			if !hasProblem(invalid, tt.key, tt.want) {
				t.Fatalf("problems = %#v", invalid.Problems)
			}
		})
	}
}

func TestLoadReportsEveryMissingField(t *testing.T) {
	_, err := load(func(string) string { return "" })
	var invalid *InvalidConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v", err)
	}

	required := []string{
		"HTTP_ADDR",
		"DATABASE_URL",
		"REDIS_ADDR",
		"OAUTH_ISSUER",
		"OAUTH_CLIENT_ID",
		"OAUTH_CLIENT_SECRET",
		"OAUTH_REDIRECT_URI",
		"OAUTH_AUDIENCE",
		"DEFAULT_MAX_RUNNING_SANDBOXES",
		"DEFAULT_CREATION_RATE_LIMIT",
		"RATE_LIMIT_WINDOW",
		"OAUTH_STATE_TTL",
		"REQUEST_TIMEOUT",
	}
	for _, name := range required {
		if !hasProblem(invalid, name, "is required") {
			t.Fatalf("missing %s in %#v", name, invalid.Problems)
		}
	}
}

func TestLoadReportsCombinedFailures(t *testing.T) {
	env := validEnv()
	delete(env, "OAUTH_CLIENT_SECRET")
	env["DEFAULT_MAX_RUNNING_SANDBOXES"] = "nope"
	env["OAUTH_STATE_TTL"] = "0s"

	_, err := load(envGetter(env))
	var invalid *InvalidConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v", err)
	}
	if !hasProblem(invalid, "OAUTH_CLIENT_SECRET", "is required") ||
		!hasProblem(invalid, "DEFAULT_MAX_RUNNING_SANDBOXES", "must be a positive integer") ||
		!hasProblem(invalid, "OAUTH_STATE_TTL", "must be a positive duration") {
		t.Fatalf("problems = %#v", invalid.Problems)
	}
}

func validEnv() map[string]string {
	return map[string]string{
		"HTTP_ADDR":                     ":8080",
		"DATABASE_URL":                  "postgres://e2b:e2b@localhost:5432/e2b",
		"REDIS_ADDR":                    "localhost:6379",
		"OAUTH_ISSUER":                  "http://localhost:4444",
		"OAUTH_CLIENT_ID":               "e2b-assignment",
		"OAUTH_CLIENT_SECRET":           "e2b-assignment-secret",
		"OAUTH_REDIRECT_URI":            "http://localhost:8080/auth/callback",
		"OAUTH_AUDIENCE":                "urn:e2b:sandbox-api",
		"DEFAULT_MAX_RUNNING_SANDBOXES": "10",
		"DEFAULT_CREATION_RATE_LIMIT":   "20",
		"RATE_LIMIT_WINDOW":             "60s",
		"OAUTH_STATE_TTL":               "5m",
		"REQUEST_TIMEOUT":               "5s",
	}
}

func envGetter(env map[string]string) func(string) string {
	return func(key string) string {
		return env[key]
	}
}

func hasProblem(err *InvalidConfigError, name, detail string) bool {
	for _, problem := range err.Problems {
		if problem.Name == name && strings.Contains(problem.Detail, detail) {
			return true
		}
	}
	return false
}
