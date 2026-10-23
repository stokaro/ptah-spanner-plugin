package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
)

func TestDescription_ValidatesWithoutRequiredFeatures(t *testing.T) {
	c := qt.New(t)
	c.Assert(provider.ValidateDescription(backend.Description(), nil), qt.IsNil)
}

func TestDescription_DeclaresNoFeature(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()
	c.Assert(description.GetFeatures(), qt.HasLen, 0)
	c.Assert(description.GetEngines(), qt.HasLen, 1)
	c.Assert(description.GetEngines()[0].GetFeatures(), qt.HasLen, 0)
}

func TestDescription_DeclaresTheEngine(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	c.Assert(description.GetProviderId(), qt.Equals, "github.com/stokaro/ptah-spanner-plugin")
	c.Assert(engine.GetId(), qt.Equals, "spanner")
	c.Assert(engine.GetAliases(), qt.DeepEquals, []string{"cloudspanner", "google-spanner", "google_spanner"})
	c.Assert(engine.GetIdentifiers().GetTableNames(), qt.Equals, protocol.IdentifierRules_ASCII_CASE_INSENSITIVE)
	c.Assert(engine.GetIdentifiers().GetColumnNames(), qt.Equals, protocol.IdentifierRules_ASCII_CASE_INSENSITIVE)
	c.Assert(engine.GetIdentifiers().GetIndexNames(), qt.Equals, protocol.IdentifierRules_ASCII_CASE_INSENSITIVE)
	c.Assert(engine.GetIdentifiers().GetIndexNamespace(), qt.Equals, protocol.IdentifierRules_SCHEMA)
}

func TestDescription_ReleaseLinesAreBestEffortWithAReason(t *testing.T) {
	c := qt.New(t)
	lines := backend.Description().GetEngines()[0].GetReleaseLines()
	c.Assert(lines, qt.Not(qt.HasLen), 0)
	for _, line := range lines {
		c.Assert(line.GetSupport(), qt.Equals, protocol.ReleaseLine_BEST_EFFORT, qt.Commentf("line %s", line.GetLine()))
		c.Assert(line.GetUnprobed(), qt.Not(qt.Equals), "", qt.Commentf("line %s", line.GetLine()))
		c.Assert(line.GetImage(), qt.Equals, "", qt.Commentf("line %s", line.GetLine()))
	}
}

func TestDescription_ReleaseLinesAreNewestFirst(t *testing.T) {
	c := qt.New(t)
	lines := backend.Description().GetEngines()[0].GetReleaseLines()
	got := make([]string, len(lines))
	for i, line := range lines {
		got[i] = line.GetLine()
	}
	c.Assert(got, qt.DeepEquals, []string{"0"})
}

func TestServer_ValidatesWithTheSessionServiceOnly(t *testing.T) {
	c := qt.New(t)
	server := runtime.Server{Description: backend.Description(), Sessions: backend.New()}
	c.Assert(server.Validate(), qt.IsNil)
}

func TestServer_RefusesAServiceTheDescriptionDoesNotAdvertise(t *testing.T) {
	c := qt.New(t)
	description := backend.Description()
	description.Features = []string{provider.InspectFeature}
	server := runtime.Server{Description: description, Sessions: backend.New()}
	c.Assert(server.Validate(), qt.ErrorIs, provider.ErrProtocol)
}

func TestOpen_FailurePath(t *testing.T) {
	c := qt.New(t)
	session, err := backend.New().Open(context.Background(), &protocol.OpenRequest{Engine: "spanner"})
	c.Assert(session, qt.IsNil)
	c.Assert(status.Code(err), qt.Equals, codes.Unimplemented)
}

func TestClose_FailurePath(t *testing.T) {
	c := qt.New(t)
	err := backend.New().Close(context.Background(), &protocol.CloseRequest{SessionId: "any"})
	c.Assert(status.Code(err), qt.Equals, codes.NotFound)
}
