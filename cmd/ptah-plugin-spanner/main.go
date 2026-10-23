// Command ptah-plugin-spanner serves Spanner through the experimental Ptah
// protocol. It implements no services yet: it negotiates, declares its engine,
// and refuses every session.
package main

import (
	"fmt"
	"os"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah/provider-sdk/runtime"
)

func main() {
	if err := runtime.Serve(runtime.Server{Description: backend.Description(), Sessions: backend.New()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
