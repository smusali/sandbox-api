package config

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config is the process environment required to start the API.
type Config struct {
	HTTPAddr                   string
	DatabaseURL                string
	RedisAddr                  string
	OAuthIssuer                string
	OAuthClientID              string
	OAuthClientSecret          string
	OAuthRedirectURI           string
	OAuthAudience              string
	DefaultMaxRunningSandboxes int
	DefaultCreationRateLimit   int
	RateLimitWindow            time.Duration
	OAuthStateTTL              time.Duration
	RequestTimeout             time.Duration
}

// Problem is one rejected configuration field.
type Problem struct {
	Name   string
	Detail string
}

// InvalidConfigError lists every configuration field rejected at startup.
type InvalidConfigError struct {
	Problems []Problem
}

func (e *InvalidConfigError) Error() string {
	var b strings.Builder
	b.WriteString("invalid configuration:")
	for _, problem := range e.Problems {
		b.WriteString("\n")
		b.WriteString(problem.Name)
		b.WriteString(": ")
		b.WriteString(problem.Detail)
	}
	return b.String()
}

// Load reads and validates configuration from the process environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	var problems []Problem

	required := func(name string) string {
		value := getenv(name)
		if value == "" {
			problems = append(problems, Problem{Name: name, Detail: "is required"})
		}
		return value
	}

	cfg := Config{
		HTTPAddr:          required("HTTP_ADDR"),
		DatabaseURL:       required("DATABASE_URL"),
		RedisAddr:         required("REDIS_ADDR"),
		OAuthIssuer:       required("OAUTH_ISSUER"),
		OAuthClientID:     required("OAUTH_CLIENT_ID"),
		OAuthClientSecret: required("OAUTH_CLIENT_SECRET"),
		OAuthRedirectURI:  required("OAUTH_REDIRECT_URI"),
		OAuthAudience:     required("OAUTH_AUDIENCE"),
	}

	if cfg.HTTPAddr != "" {
		if detail := validateListenAddr(cfg.HTTPAddr); detail != "" {
			problems = append(problems, Problem{Name: "HTTP_ADDR", Detail: detail})
		}
	}
	if cfg.DatabaseURL != "" {
		if detail := validateDatabaseURL(cfg.DatabaseURL); detail != "" {
			problems = append(problems, Problem{Name: "DATABASE_URL", Detail: detail})
		}
	}
	if cfg.RedisAddr != "" {
		if detail := validateHostPort(cfg.RedisAddr); detail != "" {
			problems = append(problems, Problem{Name: "REDIS_ADDR", Detail: detail})
		}
	}
	if cfg.OAuthIssuer != "" {
		if detail := validateIssuer(cfg.OAuthIssuer); detail != "" {
			problems = append(problems, Problem{Name: "OAUTH_ISSUER", Detail: detail})
		}
	}
	if cfg.OAuthRedirectURI != "" {
		if detail := validateRedirectURI(cfg.OAuthRedirectURI); detail != "" {
			problems = append(problems, Problem{Name: "OAUTH_REDIRECT_URI", Detail: detail})
		}
	}
	if cfg.OAuthAudience != "" {
		if detail := validateAudience(cfg.OAuthAudience, cfg.OAuthClientID); detail != "" {
			problems = append(problems, Problem{Name: "OAUTH_AUDIENCE", Detail: detail})
		}
	}

	cfg.DefaultMaxRunningSandboxes = positiveInt(required("DEFAULT_MAX_RUNNING_SANDBOXES"), "DEFAULT_MAX_RUNNING_SANDBOXES", &problems)
	cfg.DefaultCreationRateLimit = positiveInt(required("DEFAULT_CREATION_RATE_LIMIT"), "DEFAULT_CREATION_RATE_LIMIT", &problems)
	cfg.RateLimitWindow = positiveDuration(required("RATE_LIMIT_WINDOW"), "RATE_LIMIT_WINDOW", &problems)
	cfg.OAuthStateTTL = positiveDuration(required("OAUTH_STATE_TTL"), "OAUTH_STATE_TTL", &problems)
	cfg.RequestTimeout = positiveDuration(required("REQUEST_TIMEOUT"), "REQUEST_TIMEOUT", &problems)

	if len(problems) > 0 {
		return Config{}, &InvalidConfigError{Problems: problems}
	}
	return cfg, nil
}

// AuthorizationEndpoint is the Hydra authorization endpoint derived from the issuer.
func (c Config) AuthorizationEndpoint() string {
	return c.endpoint("/oauth2/auth")
}

// TokenEndpoint is the Hydra token endpoint derived from the issuer.
func (c Config) TokenEndpoint() string {
	return c.endpoint("/oauth2/token")
}

// JWKSEndpoint is the Hydra JSON Web Key Set derived from the issuer.
func (c Config) JWKSEndpoint() string {
	return c.endpoint("/.well-known/jwks.json")
}

func (c Config) endpoint(path string) string {
	issuer, err := url.Parse(c.OAuthIssuer)
	if err != nil {
		return ""
	}
	issuer.Path = path
	return issuer.String()
}

func validateListenAddr(raw string) string {
	return validateHostPort(raw)
}

func validateHostPort(raw string) string {
	_, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "must be a host:port address"
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "must include a TCP port from 1 to 65535"
	}
	return ""
}

func validateDatabaseURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Opaque != "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "must be an absolute postgres or postgresql URL"
	}
	return ""
}

func validateIssuer(raw string) string {
	if strings.HasSuffix(raw, "/") {
		return "must not have a trailing slash"
	}
	parsed, err := url.Parse(raw)
	if err != nil || !absoluteHTTP(parsed) || parsed.Path != "" {
		return "must be an absolute http or https URL without userinfo, path, query, or fragment"
	}
	return ""
}

func validateRedirectURI(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || !absoluteHTTP(parsed) {
		return "must be an absolute http or https URL without userinfo, query, or fragment"
	}
	return ""
}

func absoluteHTTP(parsed *url.URL) bool {
	if parsed == nil || parsed.Host == "" || parsed.Opaque != "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return true
}

func validateAudience(raw, clientID string) string {
	if raw == "<verified-api-audience>" || strings.ContainsAny(raw, "<>") {
		return "must be a concrete audience"
	}
	if strings.ContainsFunc(raw, unicode.IsSpace) {
		return "must not contain whitespace"
	}
	if clientID != "" && raw == clientID {
		return "must be distinct from the OAuth client id"
	}
	return ""
}

func positiveInt(raw, name string, problems *[]Problem) int {
	if raw == "" {
		return 0
	}
	number, err := strconv.Atoi(raw)
	if err != nil || number <= 0 {
		*problems = append(*problems, Problem{Name: name, Detail: "must be a positive integer"})
		return 0
	}
	return number
}

func positiveDuration(raw, name string, problems *[]Problem) time.Duration {
	if raw == "" {
		return 0
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		*problems = append(*problems, Problem{Name: name, Detail: "must be a positive duration"})
		return 0
	}
	return duration
}
