// Package db provides the Postgres connection pool, the retrying transaction
// helper used by every multi-statement write, and role-scoped pools for the four
// application database roles (spec section 16).
package db
