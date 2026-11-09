package backend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/tablefacets"
)

// CompareModels compares the row deletion policies of the tables both sides
// hold; see tablefacets.Compare. It reads no database.
func (*Provider) CompareModels(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	if err := provider.ValidateComparisonRequest(Description(), request); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	result, err := tablefacets.Compare(ctx, request)
	if err != nil {
		return nil, facetError(ctx, err)
	}
	if err := provider.ValidateComparedModelsReply(Description(), request, result); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// ReverseChanges undoes the row deletion policy changes the comparison
// returned; see tablefacets.Reverse. It reads no database.
func (*Provider) ReverseChanges(ctx context.Context, request *protocol.ReverseChangesRequest) (*protocol.ReversedChanges, error) {
	if err := provider.ValidateReverseChangesRequest(Description(), request); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	result, err := tablefacets.Reverse(ctx, request)
	if err != nil {
		return nil, facetError(ctx, err)
	}
	if err := provider.ValidateReversedChangesReply(Description(), request, result); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// ReportValues counts the row deletion policies the host captured for
// `ptah schema stats`; see tablefacets.Report.
func (*Provider) ReportValues(ctx context.Context, request *protocol.ReportValuesRequest) (*protocol.ValueReports, error) {
	if err := provider.ValidateReportValuesRequest(Description(), request); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	result, err := tablefacets.Report(ctx, request)
	if err != nil {
		return nil, facetError(ctx, err)
	}
	return result, nil
}

func facetError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
}
