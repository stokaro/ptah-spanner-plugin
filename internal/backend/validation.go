package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
)

// ValidateSchema checks the declared tables offline and reports each one the
// provider cannot create on Spanner, with the reason. It reads no database.
func (*Provider) ValidateSchema(ctx context.Context, request *protocol.ValidateSchemaRequest) (*protocol.SchemaValidation, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result := &protocol.SchemaValidation{Complete: true}
	for _, problem := range tableplan.Validate(request) {
		result.Diagnostics = append(result.Diagnostics, &protocol.SchemaDiagnostic{Code: problem.Code, Kind: problem.Kind, Object: problem.Object, Message: problem.Message})
	}
	return result, nil
}
