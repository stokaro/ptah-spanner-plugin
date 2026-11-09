package backend

import (
	"context"

	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
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
	for _, refusal := range commonobject.Refusals(request.GetCommonObjects()) {
		result.Diagnostics = append(result.Diagnostics, &protocol.SchemaDiagnostic{Code: protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, Kind: refusal.Kind,
			Object: refusal.Object, Message: refusal.Reason})
	}
	return result, nil
}
