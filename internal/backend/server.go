package backend

import "github.com/stokaro/ptah/provider-sdk/runtime"

// Server joins the description with the services that implement it. Each
// service the description offers has an implementation, and runtime.Serve
// refuses a server where the two differ.
func Server() runtime.Server {
	spanner := New()
	return runtime.Server{
		Description:     Description(),
		Sessions:        spanner,
		Inspector:       spanner,
		Renderer:        spanner,
		Executor:        spanner,
		SQLAnalyzer:     spanner,
		ValueNormalizer: spanner,
		Locks:           spanner,
		SchemaValidator: spanner,
		TablePreparer:   spanner,
		TableCreations:  spanner,
		Columns:         spanner,
		FeaturePlanner:  spanner,
		SourceDecoder:   spanner,
		SourceEncoder:   spanner,
		ValueConverter:  spanner,
		ModelComparer:   spanner,
		ChangeReverser:  spanner,
		ValueReporter:   spanner,
	}
}
