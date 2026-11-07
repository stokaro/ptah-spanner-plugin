package backend_test

import (
	"encoding/json"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// capabilityGolden is internal/backend/testdata/offline-capabilities.golden.json,
// which testdata/capabilityoracle recorded from a host that built in a Spanner
// engine: the facts that host planned the spanner dialect with.
type capabilityGolden struct {
	HostCommit   string          `json:"host_commit"`
	Preset       string          `json:"preset"`
	Capabilities map[string]bool `json:"capabilities"`
}

func readCapabilityGolden(c *qt.C) capabilityGolden {
	c.Helper()
	data, err := os.ReadFile("testdata/offline-capabilities.golden.json")
	c.Assert(err, qt.IsNil)
	var golden capabilityGolden
	c.Assert(json.Unmarshal(data, &golden), qt.IsNil)
	c.Assert(golden.Preset, qt.Equals, "SpannerPostgres")
	return golden
}

// A host that lists engine.default-capabilities plans the engine offline, and
// with a --server-version of its one line, with the facts of the built-in
// SpannerPostgres preset, as it did while it built in a Spanner engine.
func TestDescription_StatesTheOfflineCapabilities(t *testing.T) {
	c := testcheck.New(t)
	golden := readCapabilityGolden(c)
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.DefaultCapabilitiesFeature})
	c.Assert(provider.ValidateDescription(listed, nil), qt.IsNil)
	engine := listed.GetEngines()[0]
	c.Assert(engine.GetFeatures(), qt.Contains, provider.DefaultCapabilitiesFeature)
	c.Assert(engine.GetDefaultCapabilities(), qt.DeepEquals, golden.Capabilities)
	c.Assert(engine.GetReleaseLines(), qt.HasLen, 1)
	c.Assert(engine.GetReleaseLines()[0].GetCapabilities(), qt.DeepEquals, golden.Capabilities)
}

// Spanner refuses CREATE INDEX CONCURRENTLY, and the offline facts say so as
// an established false, so `ptah sql lint --dialect spanner` reports CAP001
// for the statement without a server.
func TestDescription_EstablishesThatConcurrentIndexesAreRefused(t *testing.T) {
	c := testcheck.New(t)
	listed := provider.NegotiatedDescription(backend.Description(), []string{provider.DefaultCapabilitiesFeature})
	concurrent, established := listed.GetEngines()[0].GetDefaultCapabilities()["create_index_concurrently"]
	c.Assert(established, qt.IsTrue)
	c.Assert(concurrent, qt.IsFalse)
}

// A host that does not list the feature receives no fact, neither the
// engine's nor its line's.
func TestDescription_StatesNoCapabilityToAHostWithoutTheFeature(t *testing.T) {
	c := testcheck.New(t)
	unlisted := provider.NegotiatedDescription(backend.Description(), nil)
	c.Assert(provider.ValidateDescription(unlisted, nil), qt.IsNil)
	engine := unlisted.GetEngines()[0]
	c.Assert(engine.GetFeatures(), qt.Not(qt.Contains), provider.DefaultCapabilitiesFeature)
	c.Assert(engine.GetDefaultCapabilities(), qt.IsNil)
	c.Assert(engine.GetReleaseLines()[0].GetCapabilities(), qt.IsNil)
}
