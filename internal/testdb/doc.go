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
// Test databases are named recon_test_<pid>_... for the same reason: a
// package's orphan assertion only sees clones created by its own test binary.
// CleanupOrphans removes every recon_test_% database and CleanupOrphanLogins
// every recon_test_% role for maintenance; do not run either while integration
// tests are in flight.
package testdb
