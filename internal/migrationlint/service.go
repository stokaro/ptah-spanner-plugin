package migrationlint

import (
	"context"
	"slices"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// The functions in this file answer the SDK's lint.migrations requests. The
// SDK validates a request before it arrives and a reply before it leaves.

// SplitRequest answers a SplitLintRequest: one file per source, in request
// order. Every statement carries its effects of the facts judged lists, and
// the reply says they were judged: [StatementFacts] for a host that negotiated
// lint.statement-facts, and none for one that did not.
func SplitRequest(ctx context.Context, request *protocol.SplitLintRequest, judged []protocol.LintFact) (*protocol.SplitLint, error) {
	result := &protocol.SplitLint{Judged: slices.Clone(judged)}
	for _, source := range request.GetSources() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		statements, err := Split(source.GetSql())
		if err != nil {
			return nil, err
		}
		file := &protocol.LintFile{Name: source.GetName(), Version: source.GetVersion(), Up: source.GetUp(), Down: source.GetDown(),
			Repeatable: source.GetRepeatable()}
		for _, statement := range statements {
			wire := wireStatement(statement)
			wire.Effects = wireEffects(asked(Describe(statement), judged))
			file.Statements = append(file.Statements, wire)
		}
		result.Files = append(result.Files, file)
	}
	return result, nil
}

// AnnotateRequest answers an AnnotateLintRequest over statements a host split
// without a dialect. A statement this package splits at the same bytes is
// judged for the facts the request asks for; any other is declined, because
// Spanner reads that text otherwise. A source the provider's lexer cannot read
// has every statement declined.
//
// Of the facts a host asks for, the provider reports only the statement facts:
// Spanner has no statement whose drop loses state a new object of the same
// name does not get back, no unique index outside the CREATE UNIQUE INDEX the
// host reads itself, no table rebuild, and no statement whose safety only the
// provider can judge.
func AnnotateRequest(ctx context.Context, request *protocol.AnnotateLintRequest) (*protocol.AnnotatedLint, error) {
	described := slices.ContainsFunc(request.GetFacts(), func(fact protocol.LintFact) bool { return slices.Contains(StatementFacts, fact) })
	result := &protocol.AnnotatedLint{}
	for i, file := range request.GetFiles() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		own, err := Split(request.GetSources()[i].GetSql())
		answer := &protocol.LintAnnotations{}
		for _, statement := range file.GetStatements() {
			position := slices.IndexFunc(own, func(candidate Statement) bool {
				return uint32(candidate.Start) == statement.GetStart() && uint32(candidate.End) == statement.GetEnd() // #nosec G115 -- a source the SDK accepted is far below 4 GiB
			})
			if err != nil || position < 0 {
				answer.Statements = append(answer.Statements, &protocol.LintAnnotation{Declined: true})
				continue
			}
			annotation := &protocol.LintAnnotation{}
			if described {
				annotation.Effects = wireEffects(asked(Describe(own[position]), request.GetFacts()))
			}
			answer.Statements = append(answer.Statements, annotation)
		}
		result.Files = append(result.Files, answer)
	}
	return result, nil
}

// asked keeps the effects of the facts a request asks for.
func asked(effects []Effect, facts []protocol.LintFact) []Effect {
	return slices.DeleteFunc(effects, func(effect Effect) bool { return !slices.Contains(facts, effect.Fact) })
}

// CheckRequest answers a CheckLintRequest with every finding of the rules it
// enables over every file it holds.
func CheckRequest(ctx context.Context, request *protocol.CheckLintRequest) (*protocol.CheckedLint, error) {
	files := make([]File, 0, len(request.GetFiles()))
	for _, file := range request.GetFiles() {
		checked := File{}
		for _, statement := range file.GetStatements() {
			checked.Statements = append(checked.Statements, Statement{Start: int(statement.GetStart()), End: int(statement.GetEnd()),
				Line: int(statement.GetLine()), SQL: statement.GetSql(), Canonical: statement.GetCanonical(),
				Words: slices.Clone(statement.GetWords()), SourceWords: slices.Clone(statement.GetSourceWords()),
				Comments: slices.Clone(statement.GetComments())})
		}
		files = append(files, checked)
	}
	findings, err := Check(ctx, request.GetCodes(), files)
	if err != nil {
		return nil, err
	}
	result := &protocol.CheckedLint{Complete: true}
	for _, finding := range findings {
		result.Findings = append(result.Findings, &protocol.LintFinding{Code: finding.Code, File: int32(finding.File), // #nosec G115 -- positions index the request's own lists
			Statement: int32(finding.Statement), Message: finding.Message}) // #nosec G115 -- positions index the request's own lists
	}
	return result, nil
}

func wireStatement(statement Statement) *protocol.LintStatement {
	return &protocol.LintStatement{
		Start: uint32(statement.Start), End: uint32(statement.End), Line: uint32(statement.Line), // #nosec G115 -- sqltext.Tokens refuses a text longer than math.MaxUint32
		Sql: statement.SQL, Canonical: statement.Canonical, Words: slices.Clone(statement.Words),
		SourceWords: slices.Clone(statement.SourceWords), Comments: slices.Clone(statement.Comments),
	}
}

func wireEffects(effects []Effect) []*protocol.LintEffect {
	var result []*protocol.LintEffect
	for _, effect := range effects {
		result = append(result, &protocol.LintEffect{Fact: effect.Fact, StatementClass: effect.Class, Kind: effect.Kind, Object: effect.Object,
			Table: effect.Table, Clause: effect.Clause, Capability: effect.Capability})
	}
	return result
}
