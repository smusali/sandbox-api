package db

import "embed"

// Migrations is the versioned Goose schema.
//
//go:embed migrations/*.sql
var Migrations embed.FS

//go:generate go tool sqlc generate -f ../sqlc.yaml
