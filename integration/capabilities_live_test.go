//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A session states the facts the provider plans with, measured on the server,
// and the offline facts an engine description states for a command without a
// server agree with each of them, so a command plans with one answer whether
// it connects or not.
func TestSessionsSubprocess_AgreeWithTheOfflineCapabilities(t *testing.T) {
	c := testcheck.New(t)
	session := openSession(c, startProvider(c), freshDatabase(c))
	offline := backend.Description().GetEngines()[0].GetDefaultCapabilities()
	c.Assert(session.GetCapabilities(), qt.Not(qt.HasLen), 0)
	for name, value := range session.GetCapabilities() {
		stated, established := offline[name]
		c.Assert(established, qt.IsTrue, qt.Commentf("capability %s", name))
		c.Assert(stated, qt.Equals, value, qt.Commentf("capability %s", name))
	}
}
