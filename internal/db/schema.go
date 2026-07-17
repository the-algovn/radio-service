// Package db holds the sqlc-generated data-access layer and the embedded goose
// migrations that are the single source of truth for radio-lab's schema. sqlc
// reads migrations/ (the -- +goose Up sections); cmd/radio-lab-migrate applies
// them. The service never touches schema. Mirrors the-button-service.
package db

import "embed"

// Migrations is rooted at internal/db/; callers want fs.Sub(Migrations, "migrations").
//
//go:embed migrations/*.sql
var Migrations embed.FS
