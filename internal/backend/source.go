package backend

import (
	"context"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/policysource"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
)

// DecodeSource reads the row deletion policy each Go table directive, YAML
// table entry, HCL platform block and SQL TTL clause declares; see
// policysource. It reads no file and needs no session. A document claims
// every model it enrolls completely, so a Go, YAML or SQL table without a
// policy declares none; the host reads an HCL document's claim at the tables
// that write the block alone. Any refused fragment refuses the whole batch.
func (*Provider) DecodeSource(ctx context.Context, request *protocol.DecodeSourceRequest) (*protocol.DecodedSource, error) {
	if err := provider.ValidateSourceRequest(Description(), request); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	result := &protocol.DecodedSource{Complete: true}
	for _, document := range request.GetDocuments() {
		decoded := &protocol.DecodedSourceDocument{Id: document.GetId(), Coverage: &protocol.ModelCoverage{Representation: protocol.ValueInput_DESIRED}}
		for _, model := range document.GetModels() {
			decoded.Coverage.Kinds = append(decoded.Coverage.Kinds, &protocol.ModelKindCoverage{Model: proto.CloneOf(model),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
		}
		for _, fragment := range document.GetFragments() {
			if err := ctx.Err(); err != nil {
				return nil, status.FromContextError(err).Err()
			}
			if !policysource.Owns(fragment) {
				return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the source carries a declaration the provider does not read")
			}
			table, policy, err := policysource.Decode(document, fragment)
			if err != nil {
				return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
			}
			value, err := rowdeletion.Facet(fragment.GetId(), protocol.ValueInput_DESIRED, table, policy)
			if err != nil {
				return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
			}
			decoded.Fragments = append(decoded.Fragments, &protocol.DecodedSourceFragment{Id: fragment.GetId(), Values: []*protocol.SourceModelValue{{
				Placement: protocol.SourceModelValue_FACET, Value: value,
				Source: &protocol.SourceTrace{DocumentId: document.GetId(), FragmentId: fragment.GetId(), Root: protocol.SourceTrace_CONTENT},
			}}})
			decoded.Coverage.Subjects = append(decoded.Coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: rowdeletion.Kind,
				Subject: proto.CloneOf(table), NormalizedSubject: proto.CloneOf(value.GetNormalizedOwner()),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
		}
		result.Documents = append(result.Documents, decoded)
	}
	if err := provider.ValidateDecodedSourceReply(Description(), request, result); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// EncodeSource writes each table's row deletion policy in the format the host
// exports: the table directive's platform attributes in Go, the spanner
// platform group in YAML, and the platform "spanner" block in HCL. The
// interval keeps the spelling the value carries, which for a read is the
// server's stored one.
func (*Provider) EncodeSource(ctx context.Context, request *protocol.EncodeSourceRequest) (*protocol.EncodedSource, error) {
	if err := provider.ValidateEncodeSourceRequest(Description(), request); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	var claim func(fragment *protocol.SourceFragment)
	switch request.GetFormat() {
	case protocol.SourceDocument_GO_ANNOTATIONS:
		claim = func(fragment *protocol.SourceFragment) {
			fragment.GoDirective = policysource.GoDirectives()[0]
			fragment.Selector = fragment.GoDirective.GetSelector()
		}
	case protocol.SourceDocument_YAML:
		claim = func(fragment *protocol.SourceFragment) {
			fragment.YamlKey = policysource.YAMLKeys()[0]
			fragment.Selector = fragment.YamlKey.GetSelector()
		}
	case protocol.SourceDocument_HCL:
		claim = func(fragment *protocol.SourceFragment) {
			fragment.HclBlock = policysource.HCLPlatformBlock()
			fragment.Selector = fragment.HclBlock.GetSelector()
		}
	default:
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider writes Go, YAML and HCL sources")
	}
	result := &protocol.EncodedSource{Complete: true}
	for _, value := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		policy, err := rowdeletion.PolicyOf(value.GetValue())
		if err != nil {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		fragment := &protocol.SourceFragment{Id: "policy-" + strconv.Itoa(len(result.Fragments)), BindingId: value.GetBindingId(),
			Content: policysource.Content(request.GetFormat(), policy)}
		claim(fragment)
		result.Fragments = append(result.Fragments, &protocol.EncodedFragment{Fragment: fragment, ValueIds: []string{value.GetValue().GetId()}})
	}
	if err := provider.ValidateEncodedSourceReply(Description(), request, result); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}
