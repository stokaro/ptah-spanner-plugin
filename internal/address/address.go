// Package address reads the address of a Spanner database, as Ptah passes it to
// a session, into the PostgreSQL-wire connection string PGAdapter accepts.
//
// A Spanner address is a PostgreSQL URL under one of the engine's schemes:
// spanner://host:port/database?sslmode=disable. PGAdapter speaks the
// PostgreSQL protocol in front of Spanner, so the provider connects with the
// same host, port, credentials, database and parameters under the postgres
// scheme.
package address

import (
	"errors"
	"net/url"
	"slices"
	"strings"
)

// Schemes are the URL schemes a Spanner address may carry. They are the
// schemes the Ptah host catalog recognizes for Spanner.
var Schemes = []string{"spanner", "cloudspanner", "google-spanner"}

// ErrInvalid reports an address the provider cannot connect with. Its text
// never quotes the address, because an address can carry a credential.
var ErrInvalid = errors.New("a Spanner address is spanner://host:port/database, with an optional query string")

// Target is a parsed address.
type Target struct {
	// ConnString is the address under the postgres scheme, which the
	// PostgreSQL driver parses.
	ConnString string
	// Database is the database the address names.
	Database string
}

// Parse reads a Spanner address. It refuses an address under another scheme,
// one without a host, one that names no database or a path below it, and one
// with a fragment.
func Parse(address string) (Target, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return Target{}, ErrInvalid
	}
	if !knownScheme(parsed.Scheme) || parsed.Opaque != "" || parsed.Host == "" || parsed.Fragment != "" {
		return Target{}, ErrInvalid
	}
	database := strings.TrimPrefix(parsed.Path, "/")
	if database == "" || strings.Contains(database, "/") {
		return Target{}, ErrInvalid
	}
	parsed.Scheme = "postgres"
	return Target{ConnString: parsed.String(), Database: database}, nil
}

func knownScheme(scheme string) bool {
	return slices.ContainsFunc(Schemes, func(known string) bool { return strings.EqualFold(scheme, known) })
}
