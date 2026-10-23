package releasemanifest_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/releasemanifest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

func TestBuild_HappyPath(t *testing.T) {
	c := qt.New(t)
	first, err := releasemanifest.Build(backend.Description())
	c.Assert(err, qt.IsNil)
	second, err := releasemanifest.Build(backend.Description())
	c.Assert(err, qt.IsNil)
	c.Assert(string(first), qt.Equals, string(second))
	c.Assert(string(first[len(first)-1:]), qt.Equals, "\n")

	var manifest releasemanifest.Manifest
	c.Assert(json.Unmarshal(first, &manifest), qt.IsNil)
	c.Assert(manifest.Provider, qt.Equals, backend.ProviderID)
	c.Assert(manifest.Engines, qt.HasLen, 1)
	c.Assert(manifest.Engines[0].ID, qt.Equals, "spanner")
	c.Assert(manifest.Engines[0].Lines, qt.Not(qt.HasLen), 0)
}

func TestBuild_CertifiesNoLine(t *testing.T) {
	c := qt.New(t)
	out, err := releasemanifest.Build(backend.Description())
	c.Assert(err, qt.IsNil)
	var manifest releasemanifest.Manifest
	c.Assert(json.Unmarshal(out, &manifest), qt.IsNil)
	for _, line := range manifest.Engines[0].Lines {
		c.Assert(line.Support, qt.Equals, "best_effort", qt.Commentf("line %s", line.Line))
		c.Assert(line.Unprobed, qt.Not(qt.Equals), "", qt.Commentf("line %s", line.Line))
	}
}

func TestBuild_LeavesOutAnEngineWithoutLines(t *testing.T) {
	c := qt.New(t)
	description := &protocol.Description{ProviderId: "example.org/none", Engines: []*protocol.Engine{{Id: "none"}}}
	out, err := releasemanifest.Build(description)
	c.Assert(err, qt.IsNil)
	c.Assert(string(out), qt.Equals, "{\n  \"provider\": \"example.org/none\",\n  \"engines\": []\n}\n")
}
