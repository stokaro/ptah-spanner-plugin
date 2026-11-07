// Command ptah-plugin-spanner serves Cloud Spanner's PostgreSQL interface to
// Ptah through the experimental provider protocol. Ptah starts it as a
// subprocess; it is not a command-line tool.
package main

import (
	"fmt"
	"os"

	"github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
)

func main() {
	if err := runtime.Serve(backend.Server()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
