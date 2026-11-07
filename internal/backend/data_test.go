package backend_test

import (
	"encoding/json"
	"os"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The provider and the engine offer the row services and the seed service,
// so a declared row set and a seed run reach the provider instead of being
// refused by the host.
func TestDescription_OffersTheDataServices(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	for _, feature := range []string{provider.TypeRowsFeature, provider.ReadRowsFeature, provider.RenderRowWritesFeature, provider.SeedsFeature} {
		c.Assert(description.GetFeatures(), qt.Contains, feature)
		c.Assert(description.GetEngines()[0].GetFeatures(), qt.Contains, feature)
	}
}

// dialectGolden is internal/backend/testdata/query-dialect.golden.json, which
// testdata/oracle recorded from the host's built-in Spanner descriptor.
type dialectGolden struct {
	Placeholder string `json:"placeholder"`
	Quoting     struct {
		Style    string   `json:"style"`
		Bare     string   `json:"bare"`
		Keywords []string `json:"keywords"`
		Paths    bool     `json:"paths"`
	} `json:"quoting"`
	Query struct {
		ChecksCapabilities bool     `json:"checks_capabilities"`
		UnsupportedJoins   []string `json:"unsupported_joins"`
		OffsetFetch        bool     `json:"offset_fetch"`
		FetchOrderBy       string   `json:"fetch_order_by"`
		OffsetOnlyLimit    string   `json:"offset_only_limit"`
		UnsignedBounds     bool     `json:"unsigned_bounds"`
		Upsert             string   `json:"upsert"`
		UpsertRefusal      string   `json:"upsert_refusal"`
		UpdateRefusal      string   `json:"update_refusal"`
	} `json:"query"`
	DefaultCapabilities map[string]bool `json:"default_capabilities"`
}

// The engine declares the query-builder spelling of the host's built-in
// Spanner descriptor, so the host's query builder writes the same statements
// for the provider's engine once the built-in engine is gone, and the lexical
// rules measured on the emulator, which the built-in descriptor did not state.
func TestDescription_DeclaresTheBuiltInQueryDialect(t *testing.T) {
	c := testcheck.New(t)
	data, err := os.ReadFile("testdata/query-dialect.golden.json")
	c.Assert(err, qt.IsNil)
	var golden dialectGolden
	c.Assert(json.Unmarshal(data, &golden), qt.IsNil)
	want := &protocol.QueryDialect{
		Placeholder: golden.Placeholder,
		Quoting:     &protocol.QueryQuoting{Style: golden.Quoting.Style, Bare: golden.Quoting.Bare, Keywords: golden.Quoting.Keywords, Paths: golden.Quoting.Paths},
		Query: &protocol.QuerySpelling{ChecksCapabilities: golden.Query.ChecksCapabilities, UnsupportedJoins: golden.Query.UnsupportedJoins,
			OffsetFetch: golden.Query.OffsetFetch, FetchOrderBy: golden.Query.FetchOrderBy, OffsetOnlyLimit: golden.Query.OffsetOnlyLimit,
			UnsignedBounds: golden.Query.UnsignedBounds, Upsert: golden.Query.Upsert, UpsertRefusal: golden.Query.UpsertRefusal, UpdateRefusal: golden.Query.UpdateRefusal},
		DefaultCapabilities: golden.DefaultCapabilities,
		Lexical:             &protocol.QueryLexical{EscapeStrings: true, ContinuedStrings: true, UnicodeStrings: true, NestedComments: true},
	}
	declared := backend.Description().GetEngines()[0].GetQueryDialect()
	c.Assert(declared, qt.CmpEquals(protocmp.Transform()), want)
	c.Assert(provider.ValidateQueryDialect(declared), qt.IsNil)
}
