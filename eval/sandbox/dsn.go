package sandbox

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// parseAdminDSN validates the maintenance URL without connecting.
func parseAdminDSN(dsn string) (*url.URL, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("%w: admin DSN is required", ErrInvalidOptions)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: parse admin DSN: %v", ErrInvalidOptions, err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return nil, fmt.Errorf("%w: admin DSN must be a postgres:// URL, got scheme %q",
			ErrInvalidOptions, parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w: admin DSN has no host", ErrInvalidOptions)
	}
	return parsed, nil
}

// dsnForDatabase rewrites a DSN to point at database while preserving the
// user, query parameters, and any TLS settings.
func dsnForDatabase(dsn, database string) (string, error) {
	parsed, err := parseAdminDSN(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + database
	parsed.RawPath = ""
	return parsed.String(), nil
}

// dsnForLogin rewrites a DSN to point at database and authenticate as a
// freshly provisioned login. The result contains a password and must never be
// logged.
func dsnForLogin(dsn, database, username, password string) (string, error) {
	parsed, err := parseAdminDSN(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + database
	parsed.RawPath = ""
	parsed.User = url.UserPassword(username, password)
	return parsed.String(), nil
}

// dsnWithMaxConns sets pgxpool's pool_max_conns connection parameter.
func dsnWithMaxConns(dsn string, maxConns int32) (string, error) {
	parsed, err := parseAdminDSN(dsn)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("pool_max_conns", strconv.FormatInt(int64(maxConns), 10))
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
