package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/smusali/sandbox-api/internal/auth"
	"github.com/smusali/sandbox-api/internal/config"
	sqldb "github.com/smusali/sandbox-api/internal/db/sqlc"
	"github.com/smusali/sandbox-api/internal/httpapi"
	"github.com/smusali/sandbox-api/internal/oauth"
	"github.com/smusali/sandbox-api/internal/projects"
	"github.com/smusali/sandbox-api/internal/ratelimit"
	"github.com/smusali/sandbox-api/internal/sandboxes"
)

const idleTimeout = 60 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openPostgres(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	redisClient, err := openRedis(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		_ = redisClient.Close()
	}()

	admissionClient, err := openAdmissionRedis(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		_ = admissionClient.Close()
	}()
	handler, err := newHandler(cfg, pool, redisClient, admissionClient)
	if err != nil {
		return err
	}
	server := newHTTPServer(cfg, handler)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.RequestTimeout)
		defer cancel()
		return server.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func openPostgres(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.ConnConfig.ConnectTimeout = cfg.RequestTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func openAdmissionRedis(ctx context.Context, cfg config.Config) (*redis.Client, error) {
	client := redis.NewClient(ratelimit.ClientOptions(cfg.RedisAddr, cfg.RequestTimeout))
	pingCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping admission redis: %w", err)
	}
	return client, nil
}

func openRedis(ctx context.Context, cfg config.Config) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{
		Addr:         cfg.RedisAddr,
		DialTimeout:  cfg.RequestTimeout,
		ReadTimeout:  cfg.RequestTimeout,
		WriteTimeout: cfg.RequestTimeout,
	})

	pingCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return client, nil
}

func newHandler(cfg config.Config, pool *pgxpool.Pool, stateRedis, admission *redis.Client) (http.Handler, error) {
	limiter, err := ratelimit.New(admission, cfg.RateLimitWindow)
	if err != nil {
		return nil, err
	}
	verifier := auth.NewVerifier(
		cfg.OAuthIssuer,
		cfg.OAuthAudience,
		cfg.OAuthClientID,
		cfg.JWKSEndpoint(),
		cfg.RequestTimeout,
	)
	queries := sqldb.New(pool)
	flow := oauth.New(
		oauth.Settings{
			ClientID:     cfg.OAuthClientID,
			ClientSecret: cfg.OAuthClientSecret,
			RedirectURI:  cfg.OAuthRedirectURI,
			AuthURL:      cfg.AuthorizationEndpoint(),
			TokenURL:     cfg.TokenEndpoint(),
			Audience:     cfg.OAuthAudience,
			StateTTL:     cfg.OAuthStateTTL,
			Timeout:      cfg.RequestTimeout,
		},
		oauth.NewRedisStore(stateRedis),
		oauth.NewExchanger(cfg.OAuthClientID, cfg.OAuthClientSecret, cfg.OAuthRedirectURI, cfg.TokenEndpoint(), cfg.RequestTimeout),
		verifier,
		queries,
	)
	projectSvc := projects.New(pool, queries, projects.Limits{
		MaxRunningSandboxes: cfg.DefaultMaxRunningSandboxes,
		CreationRateLimit:   cfg.DefaultCreationRateLimit,
	}, cfg.RequestTimeout)
	projectAPI := httpapi.NewProjects(projectSvc, cfg.RequestTimeout)
	sandboxAPI := httpapi.NewSandboxes(
		sandboxes.New(pool, queries, projectSvc, limiter, cfg.RequestTimeout),
		cfg.RequestTimeout,
	)
	oauthAPI := httpapi.NewOAuth(flow)
	return httpapi.NewRouter(httpapi.Authenticate(verifier, queries, cfg.RequestTimeout), httpapi.Routes{
		Login:         oauthAPI.Login(),
		Callback:      oauthAPI.Callback(),
		CreateProject: projectAPI.Create(),
		ListProjects:  projectAPI.List(),
		AddMember:     projectAPI.AddMember(),
		CreateSandbox: sandboxAPI.Create(),
		ListSandboxes: sandboxAPI.List(),
		StopSandbox:   sandboxAPI.Stop(),
	}), nil
}

func newHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.RequestTimeout,
		ReadTimeout:       cfg.RequestTimeout,
		WriteTimeout:      cfg.RequestTimeout,
		IdleTimeout:       idleTimeout,
	}
}
