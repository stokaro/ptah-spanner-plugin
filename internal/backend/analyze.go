package backend

import (
	"context"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// AnalyzeSQL splits each source into statements and says what each one is.
// The host reads the engine's SQL through it wherever it judges a statement,
// such as the data-safety check before a plan runs. Every byte of a source is
// part of a statement or white space between statements, and a statement the
// provider does not know refuses the whole request.
func (*Provider) AnalyzeSQL(ctx context.Context, request *protocol.AnalyzeSQLRequest) (*protocol.SQLAnalysis, error) {
	if request.GetEngine() != engineID {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider analyzes the SQL of the spanner engine only")
	}
	result := &protocol.SQLAnalysis{Complete: true}
	for _, source := range request.GetSources() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		statements, err := sqltext.Split(source)
		if err != nil {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		file := &protocol.SQLFile{}
		for _, statement := range statements {
			file.Statements = append(file.Statements, &protocol.SQLStatement{
				Start:       uint32(statement.Start), // #nosec G115 -- sqltext.Split refuses a text longer than math.MaxUint32
				End:         uint32(statement.End),   // #nosec G115 -- sqltext.Split refuses a text longer than math.MaxUint32
				Line:        uint32(statement.Line),  // #nosec G115 -- a line number is at most the text length
				Words:       statement.Words,
				SourceWords: statement.SourceWords,
				Canonical:   strings.Join(statement.Words, " "),
				// Every statement the provider analyzes is common DDL: the host
				// judges what a common statement does to the schema, and a
				// DROP is destructive in its judgment, not in the provider's.
				Safety: protocol.SQLStatement_COMMON_DDL,
			})
		}
		result.Files = append(result.Files, file)
	}
	return result, nil
}
