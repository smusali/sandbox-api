package db

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Up applies every pending migration.
func Up(ctx context.Context, pool *pgxpool.Pool) error {
	return apply(ctx, pool, func(ctx context.Context, provider *goose.Provider) error {
		_, err := provider.Up(ctx)
		return err
	})
}

// Down rolls the schema back to the empty version.
func Down(ctx context.Context, pool *pgxpool.Pool) error {
	return apply(ctx, pool, func(ctx context.Context, provider *goose.Provider) error {
		_, err := provider.DownTo(ctx, 0)
		return err
	})
}

func apply(ctx context.Context, pool *pgxpool.Pool, run func(context.Context, *goose.Provider) error) error {
	migrations, err := fs.Sub(Migrations, "migrations")
	if err != nil {
		return fmt.Errorf("open migrations: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	if err := run(ctx, provider); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
