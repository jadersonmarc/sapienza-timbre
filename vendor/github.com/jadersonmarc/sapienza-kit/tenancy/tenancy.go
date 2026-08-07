// Package tenancy provides the multi-tenant primitives shared by Sapienza data
// planes: deriving a tenant's Postgres schema, provisioning it, and scoping a
// transaction to it via search_path.
//
// Golden rule: product data lives in per-tenant schemas (tenant_<id>). The
// control plane (sapienza-core) creates the empty schema; each product applies
// its own migrations (see MigrationRunner) and scopes every query with
// WithTenant. Nothing here writes to the public (control-plane) schema.
package tenancy

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SchemaName returns the Postgres schema name for a tenant: "tenant_" followed
// by the tenant UUID with hyphens removed (39 chars, well under the 63-char
// identifier limit). Deriving strictly from a parsed UUID keeps the name safe
// to interpolate into DDL.
func SchemaName(tenantID uuid.UUID) string {
	return "tenant_" + strings.ReplaceAll(tenantID.String(), "-", "")
}

// quoteSchema returns a safely-quoted identifier for the tenant schema.
func quoteSchema(tenantID uuid.UUID) string {
	return pgx.Identifier{SchemaName(tenantID)}.Sanitize()
}

// Provisioner creates tenant schemas. The control plane owns provisioning; a
// product may also call CreateSchema idempotently before applying migrations.
type Provisioner struct{}

// CreateSchema creates the tenant's schema if it does not exist. It is a no-op
// when the schema already exists. Runs inside the caller's transaction.
func (Provisioner) CreateSchema(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteSchema(tenantID)); err != nil {
		return fmt.Errorf("create schema for tenant %s: %w", tenantID, err)
	}
	return nil
}

// WithTenant scopes the given transaction to the tenant's schema by setting
// search_path to "tenant_<id>, public" for the remainder of the transaction.
// SET LOCAL only lives inside a transaction, so tx must be a real transaction.
// Every read/write of product data must run after WithTenant on the same tx.
func WithTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SET LOCAL search_path TO "+quoteSchema(tenantID)+", public"); err != nil {
		return fmt.Errorf("set search_path for tenant %s: %w", tenantID, err)
	}
	return nil
}
