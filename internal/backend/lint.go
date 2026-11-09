package backend

import (
	"context"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
)

// SplitLintStatements splits migrations as Spanner reads them and, for a host
// that lists lint.statement-facts, says what each statement is (see
// internal/migrationlint). The host reads every migration it lints, checks
// for safety before it runs one, or judges a plan's statements through this
// split, so it splits a statement of any kind. A text the provider's lexer
// cannot read fails the call and returns no statement.
func (*Provider) SplitLintStatements(ctx context.Context, request *protocol.SplitLintRequest) (*protocol.SplitLint, error) {
	var judged []protocol.LintFact
	if slices.Contains(negotiatedFeatures(ctx), provider.LintStatementFactsFeature) {
		judged = migrationlint.StatementFacts
	}
	result, err := migrationlint.SplitRequest(ctx, request, judged)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	return result, nil
}

// AnnotateLintStatements judges the statements a host split without a
// dialect, declining the ones Spanner reads otherwise.
func (*Provider) AnnotateLintStatements(ctx context.Context, request *protocol.AnnotateLintRequest) (*protocol.AnnotatedLint, error) {
	result, err := migrationlint.AnnotateRequest(ctx, request)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	return result, nil
}

// CheckLintMigrations judges every file of a run under the SP rules the
// request enables.
func (*Provider) CheckLintMigrations(ctx context.Context, request *protocol.CheckLintRequest) (*protocol.CheckedLint, error) {
	result, err := migrationlint.CheckRequest(ctx, request)
	if err != nil {
		return nil, lintError(ctx, err)
	}
	return result, nil
}

// lintError is the status of a lint call that failed: the caller's
// cancellation, or a text or a request the provider could not answer, whose
// reason says why without quoting the text.
func lintError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
}
