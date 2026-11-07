// Package migrationunits splits a migration body into the units Spanner runs
// it as, under the contract's units.migration-split feature.
//
// Spanner runs a schema statement only outside a transaction (`DDL statements
// are only allowed outside explicit transactions`), so a body cannot run in
// one transaction and each of its statements commits on its own. Each
// statement is therefore a unit. A statement that changes or reads rows runs
// in a serializable transaction of the host's together with the checkpoint
// that records it, so the two commit together or not at all. Every other
// statement runs outside a transaction: the host marks it in flight, runs it
// and checkpoints it.
//
// A unit's source is the statement as Ptah's built-in Spanner engine read it
// for its revision records: from its first token through its semicolon,
// verbatim, or to the end of the body for a last statement no semicolon ends.
// Partial hashes and failure records read that text, so a migration the
// engine left part way is resumed by the provider over the same digests.
//
// A statement that controls a transaction or a batch, or changes a session
// setting, is refused with the whole body: the host decides where each
// transaction begins and ends, and a setting would not reach the units after
// it, which may run on another connection.
package migrationunits

import (
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// Reasons the split gives for a refused body. None quotes the body: the host
// quotes the refused range itself.
const (
	// TransactionControlReason refuses a statement that opens, ends or marks
	// a transaction, or starts, runs or abandons a PGAdapter batch.
	TransactionControlReason = "the statement controls a transaction or a batch, and the host runs each statement " +
		"of a Spanner migration on its own; remove it"
	// SettingReason refuses a statement that changes a session setting.
	SettingReason = "the statement changes a session setting, which would not reach the statements after it; " +
		"remove it"
	// LexingReason refuses a body the provider cannot read into statements.
	LexingReason = "the provider cannot read the body into statements: it holds a dollar-quoted string, " +
		"an unclosed quote or comment, or a character outside Spanner's SQL"
)

// controlWords start a statement that controls a transaction or a batch:
// BEGIN, START TRANSACTION and START BATCH, COMMIT, END, ROLLBACK, ABORT,
// SAVEPOINT, RELEASE SAVEPOINT, PREPARE TRANSACTION and RUN BATCH.
var controlWords = []string{"BEGIN", "START", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE", "RUN"}

// settingWords start a statement that changes a session setting.
var settingWords = []string{"SET", "RESET"}

// dataWords start a statement that changes or reads rows and nothing else.
var dataWords = []string{"INSERT", "UPDATE", "DELETE", "SELECT", "WITH", "VALUES", "SHOW"}

// statement is one statement of a body.
type statement struct {
	// start is where its first token starts, end where its source ends, and
	// terminator where its semicolon starts, or end when none ends it.
	start, end, terminator int
	// words are its word tokens in upper case, and first its first token.
	words []string
	first sqltext.Token
}

// Split returns the units body runs as, in order, or the refusal of the whole
// body. A body with no statement has neither.
func Split(body string) ([]*protocol.MigrationUnit, *protocol.SplitRefusal) {
	statements, ok := read(body)
	if !ok {
		return nil, refused(0, len(body), LexingReason)
	}
	units := make([]*protocol.MigrationUnit, 0, len(statements))
	for _, current := range statements {
		head := current.head()
		switch {
		case slices.Contains(controlWords, head):
			return nil, refused(current.start, current.end, TransactionControlReason)
		case slices.Contains(settingWords, head):
			return nil, refused(current.start, current.end, SettingReason)
		}
		placement := protocol.MigrationUnit_OUTSIDE
		if current.data() {
			placement = protocol.MigrationUnit_WITH_CHECKPOINT
		}
		units = append(units, &protocol.MigrationUnit{
			Sql:       strings.TrimSpace(body[current.start:current.terminator]),
			Source:    body[current.start:current.end],
			Placement: placement,
		})
	}
	return units, nil
}

// ReadOnly reports whether a check assertion is one statement that reads rows
// and changes nothing, and why not when it is not.
func ReadOnly(assertion string) (bool, string) {
	statements, ok := read(assertion)
	switch {
	case !ok:
		return false, LexingReason
	case len(statements) != 1:
		return false, "the assertion is not exactly one statement"
	}
	head := statements[0].head()
	if head != "SELECT" && head != "WITH" && head != "VALUES" {
		return false, "the assertion is not a query that only reads rows"
	}
	for _, word := range statements[0].words {
		if slices.Contains([]string{"INSERT", "UPDATE", "DELETE"}, word) {
			return false, "the assertion changes rows"
		}
	}
	return true, ""
}

// head is the statement's first word in upper case, or the empty string when
// it starts with something else, such as a parenthesis.
func (s statement) head() string {
	if s.first.Kind != sqltext.Word {
		return ""
	}
	return strings.ToUpper(s.first.Text)
}

// data reports a statement that changes or reads rows and nothing else: one
// that starts with a data word, or with a parenthesis, as a query in
// parentheses does.
func (s statement) data() bool {
	return slices.Contains(dataWords, s.head()) || s.first.Kind == sqltext.Punctuation && s.first.Text == "("
}

// read splits a text into statements, leaving out the white space and
// comments between them and a stray semicolon that ends no statement. It
// reports false for a text the provider cannot tokenize.
func read(text string) ([]statement, bool) {
	tokens, err := sqltext.Tokens(text)
	if err != nil {
		return nil, false
	}
	var statements []statement
	var current *statement
	for _, token := range tokens {
		switch {
		case token.Kind == sqltext.Space || token.Kind == sqltext.Comment:
			continue
		case token.Kind == sqltext.Semicolon:
			if current != nil {
				current.terminator, current.end = token.Start, token.End
				statements = append(statements, *current)
				current = nil
			}
			continue
		case current == nil:
			current = &statement{start: token.Start, first: token}
		}
		if token.Kind == sqltext.Word {
			current.words = append(current.words, strings.ToUpper(token.Text))
		}
	}
	if current != nil {
		current.terminator, current.end = len(text), len(text)
		statements = append(statements, *current)
	}
	return statements, true
}

func refused(start, end int, reason string) *protocol.SplitRefusal {
	// #nosec G115 -- the range lies in the request's body, which the transport bounds far below math.MaxUint32
	return &protocol.SplitRefusal{Start: uint32(start), End: uint32(end), Reason: reason}
}
