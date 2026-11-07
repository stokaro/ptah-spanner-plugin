// Command spannerdialectoracle records the query-builder spelling of the Ptah
// host's built-in Spanner descriptor, so the provider's query dialect can be
// compared with it without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages that sit behind an internal boundary, so it has to be copied
// into a checkout of the host that still builds in a Spanner engine and run
// from there:
//
//	mkdir -p <host>/cmd/spannerdialectoracle
//	cp internal/backend/testdata/oracle/main.go <host>/cmd/spannerdialectoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/spannerdialectoracle <plugin>)
//
// It writes internal/backend/testdata/query-dialect.golden.json: the
// placeholder style, the quoting and query sections of the descriptor, and the
// capabilities the builder assumes for Spanner when its caller names no
// target, for the keys the builder reads.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ptah.run/core/platform"
	"ptah.run/core/platform/capability"
	"ptah.run/core/platform/descriptor"
)

type dialectGolden struct {
	HostCommit          string             `json:"host_commit"`
	Placeholder         string             `json:"placeholder"`
	Quoting             descriptor.Quoting `json:"quoting"`
	Query               descriptor.Query   `json:"query"`
	DefaultCapabilities map[string]bool    `json:"default_capabilities"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: spannerdialectoracle <plugin checkout>")
		os.Exit(2)
	}
	if err := record(os.Args[1], os.Getenv("HOST_COMMIT")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func record(root, commit string) error {
	described, ok := platform.Lookup("spanner")
	if !ok {
		return fmt.Errorf("the host describes no spanner dialect")
	}
	caps := capability.ForDialect("spanner")
	defaults := map[string]bool{}
	for _, key := range []capability.Capability{
		capability.CommonTableExpressions, capability.CorrelatedSubqueries, capability.NonEquiJoins,
		capability.OffsetWithoutLimit, capability.ReturningClause,
	} {
		if caps.Established(key) {
			defaults[string(key)] = caps.Has(key)
		}
	}
	golden := dialectGolden{HostCommit: commit, Placeholder: string(described.Placeholder), Quoting: described.Quoting,
		Query: described.Query, DefaultCapabilities: defaults}
	data, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "internal/backend/testdata/query-dialect.golden.json"), append(data, '\n'), 0o600)
}
