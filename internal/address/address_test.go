package address_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/address"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func TestParse_HappyPath(t *testing.T) {
	tests := []struct {
		name, address, connString, database string
	}{
		{name: "the spanner scheme", address: "spanner://127.0.0.1:5432/app?sslmode=disable",
			connString: "postgres://127.0.0.1:5432/app?sslmode=disable", database: "app"},
		{name: "the cloudspanner scheme", address: "cloudspanner://localhost/app",
			connString: "postgres://localhost/app", database: "app"},
		{name: "the google-spanner scheme in upper case", address: "GOOGLE-SPANNER://localhost:5433/app",
			connString: "postgres://localhost:5433/app", database: "app"},
		// #nosec G101 -- a made-up credential, carried through to the driver's address unchanged
		{name: "credentials and parameters", address: "spanner://alice:s3cret@host:5432/app?sslmode=require&application_name=ptah",
			connString: "postgres://alice:s3cret@host:5432/app?sslmode=require&application_name=ptah", database: "app"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			target, err := address.Parse(test.address)
			c.Assert(err, qt.IsNil)
			c.Assert(target.ConnString, qt.Equals, test.connString)
			c.Assert(target.Database, qt.Equals, test.database)
		})
	}
}

func TestParse_FailurePath(t *testing.T) {
	tests := []struct {
		name, address string
	}{
		{name: "another scheme", address: "postgres://host:5432/app"},
		{name: "no host", address: "spanner:///app"},
		{name: "no database", address: "spanner://host:5432"},
		{name: "an empty database", address: "spanner://host:5432/"},
		{name: "a path below the database", address: "spanner://host:5432/app/more"},
		{name: "a fragment", address: "spanner://host:5432/app#x"},
		{name: "an opaque address", address: "spanner:app"},
		{name: "a malformed address", address: "spanner://%zz/app"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			target, err := address.Parse(test.address)
			c.Assert(err, qt.ErrorIs, address.ErrInvalid)
			c.Assert(target, qt.Equals, address.Target{})
		})
	}
}

// A refusal never quotes the address, because an address can carry a
// credential.
func TestParse_RefusalQuotesNoCredential(t *testing.T) {
	c := testcheck.New(t)
	_, err := address.Parse("postgres://alice:s3cret@host/app")
	c.Assert(err, qt.ErrorIs, address.ErrInvalid)
	c.Assert(err.Error(), qt.Not(qt.Contains), "s3cret")
}
