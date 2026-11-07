package backend_test

import (
	"os"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The live job runs the image the release line names. A bump of one without
// the other would claim measurements of a server the job does not start.
func TestLiveLinesRunTheDeclaredImage(t *testing.T) {
	c := testcheck.New(t)
	script, err := os.ReadFile("../../scripts/live-lines.sh")
	c.Assert(err, qt.IsNil)
	c.Assert(strings.Count(string(script), `"image":"`), qt.Equals, 1)
	c.Assert(string(script), qt.Contains, `"image":"`+backend.EmulatorImage+`"`)
}
