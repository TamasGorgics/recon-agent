// Package testdb creates isolated Postgres databases for integration tests by
// cloning a migrated template database.
//
// Every clone gets one ephemeral LOGIN role per application group role
// (recon_app, recon_executor, recon_agent_ro, recon_audit_ro). The login name is
// unique per test database, its password is random, and it is a member of its
// group with INHERIT. DB.DSNFor returns the role-scoped DSN; cleanup drops the
// clone and every ephemeral login. The maintenance/admin connection therefore
// needs CREATEDB and CREATEROLE (the compose default user is a superuser).
//
// Login roles are named recon_test_login_<pid>_... so a package can assert that
// none survive its own test run without racing other package test binaries.
// CleanupOrphanLogins removes leftovers for maintenance; do not run it while
// integration tests are in flight.
package testdb
