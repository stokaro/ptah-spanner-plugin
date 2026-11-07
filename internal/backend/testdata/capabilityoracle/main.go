// Command spannercapabilityoracle records the capability facts the Ptah host
// planned Spanner with while it built in a Spanner engine, so the provider's
// offline capabilities can be compared with them without importing host code.
//
// It is not part of this module. Go ignores testdata, and the program imports
// host packages, so it has to be copied into a checkout of a host that still
// builds in a Spanner engine and run from there:
//
//	mkdir -p <host>/cmd/spannercapabilityoracle
//	cp internal/backend/testdata/capabilityoracle/main.go <host>/cmd/spannercapabilityoracle/main.go
//	(cd <host> && HOST_COMMIT=$(git rev-parse HEAD) go run ./cmd/spannercapabilityoracle <plugin>)
//
// It writes internal/backend/testdata/offline-capabilities.golden.json: every
// fact the host's answer for the spanner dialect establishes, which is the
// SpannerPostgres preset the dialect's descriptor names and the capability
// cell spanner-0 probed on the emulator behind PGAdapter.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"ptah.run/core/platform/capability"
)

type capabilityGolden struct {
	HostCommit   string          `json:"host_commit"`
	Preset       string          `json:"preset"`
	Capabilities map[string]bool `json:"capabilities"`
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: spannercapabilityoracle <plugin checkout>")
		os.Exit(2)
	}
	if err := record(os.Args[1], os.Getenv("HOST_COMMIT")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func record(root, commit string) error {
	planned := capability.ForDialect("spanner")
	if planned.Len() == 0 {
		return fmt.Errorf("the host plans no capability for the spanner dialect")
	}
	if !planned.Equal(capability.SpannerPostgres()) {
		return fmt.Errorf("the host plans spanner with facts other than the SpannerPostgres preset")
	}
	facts := make(map[string]bool, planned.Len())
	for key, value := range planned.All() {
		facts[string(key)] = value
	}
	golden := capabilityGolden{HostCommit: commit, Preset: "SpannerPostgres", Capabilities: facts}
	data, err := json.MarshalIndent(golden, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "internal/backend/testdata/offline-capabilities.golden.json"), append(data, '\n'), 0o600)
}
