// Command provider-manifest prints the ptah-provider.json document published
// beside each release: the provider identity, the oldest provider contract a
// host must implement to start it, and the archives the release carries. A host
// reads it to choose the newest release it can start.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/releasemanifest"
)

func main() {
	version := flag.String("version", "", "release version without the leading v")
	flag.Parse()
	out, err := releasemanifest.BuildProvider(backend.Description(), *version, "ptah-plugin-spanner", "ptah-plugin-spanner")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if _, err := os.Stdout.Write(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
