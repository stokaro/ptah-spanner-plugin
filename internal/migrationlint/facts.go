package migrationlint

import (
	"slices"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// Effect is one fact about what a statement does, as the protocol's
// LintEffect carries it.
type Effect struct {
	Fact       protocol.LintFact
	Class      protocol.LintStatementClass
	Kind       string
	Object     string
	Table      string
	Clause     string
	Capability string
}

// StatementFacts are the facts every statement of a split is judged for when
// the host negotiated lint.statement-facts: what the statement is, which table
// it creates without a primary key, and which capabilities it needs. `ptah sql
// lint` reads a Spanner source by them, since the host has no grammar for an
// engine a provider serves.
var StatementFacts = []protocol.LintFact{
	protocol.LintFact_LINT_FACT_STATEMENT_KIND, protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY,
	protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY,
}

// createIndexConcurrently is the capability CREATE INDEX CONCURRENTLY needs.
// Spanner refuses it (`Concurrent index creation is not supported in <CREATE
// INDEX> statement`, measured on the emulator), and the host's capability
// registry names it, so a host that knows the target's capabilities reports
// the statement as CAP001, as it did while Spanner was built in.
const createIndexConcurrently = "create_index_concurrently"

// Describe returns the statement facts of one statement: its kind, then a
// table it creates without a primary key, then the capabilities it needs.
//
// It reads a statement as the host's built-in Spanner path of `ptah sql lint`
// classified one: CREATE TABLE and ALTER TABLE by name, any other CREATE,
// ALTER or DROP as a schema statement named by its first two words, and every
// other statement, GRANT and REVOKE among them, as one that changes no schema
// object, named by its first word. A schema statement the host's grammar could
// not parse, such as CREATE CHANGE STREAM, ALTER INDEX or a view with SQL
// SECURITY, is a statement of its kind here, where that path refused it as
// unsupported SQL.
func Describe(statement Statement) []Effect {
	words := statement.Words
	switch {
	case len(words) == 0:
		return nil
	case hasPrefix(words, "CREATE", "TABLE"):
		table := tableAfter(statement, 2)
		effects := []Effect{kind(protocol.LintStatementClass_LINT_STATEMENT_CLASS_CREATE_TABLE, "CREATE TABLE", table)}
		if table != "" && !hasSequence(words, "PRIMARY", "KEY") {
			effects = append(effects, Effect{Fact: protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY, Table: table})
		}
		return effects
	case hasPrefix(words, "ALTER", "TABLE"):
		return []Effect{kind(protocol.LintStatementClass_LINT_STATEMENT_CLASS_ALTER_TABLE, "ALTER TABLE", tableAfter(statement, 2))}
	case words[0] == "CREATE" || words[0] == "ALTER" || words[0] == "DROP":
		effects := []Effect{kind(protocol.LintStatementClass_LINT_STATEMENT_CLASS_SCHEMA, schemaKind(words), "")}
		if requirement, found := concurrentIndex(statement); found {
			effects = append(effects, requirement)
		}
		return effects
	default:
		return []Effect{kind(protocol.LintStatementClass_LINT_STATEMENT_CLASS_OTHER, otherKind(statement), "")}
	}
}

func kind(class protocol.LintStatementClass, name, object string) Effect {
	return Effect{Fact: protocol.LintFact_LINT_FACT_STATEMENT_KIND, Class: class, Kind: name, Object: object}
}

// schemaKind names a schema statement by its first two words, as the host's
// SQL004 lists the statements no rule examined: DROP INDEX, CREATE SEQUENCE.
// A modifier between the verb and the object is skipped, so CREATE UNIQUE
// INDEX is a CREATE INDEX and CREATE OR REPLACE VIEW a CREATE VIEW, and a
// change stream is named by both its words.
func schemaKind(words []string) string {
	rest := words[1:]
	switch {
	case hasPrefix(rest, "UNIQUE"):
		rest = rest[1:]
	case hasPrefix(rest, "OR", "REPLACE"):
		rest = rest[2:]
	}
	if hasPrefix(rest, "CHANGE", "STREAM") {
		return words[0] + " CHANGE STREAM"
	}
	if len(rest) == 0 || !bare(rest[0]) {
		return words[0]
	}
	return words[0] + " " + rest[0]
}

// otherKind names a statement that changes no schema object by its first
// word, as written in upper case, or `expression` when it starts with
// something else, such as a parenthesized query.
func otherKind(statement Statement) string {
	if bare(statement.Words[0]) {
		return statement.Words[0]
	}
	return "expression"
}

// tableAfter is the table a statement names at word position, as written: a
// name, or a schema and a name joined by a dot, after an optional IF [NOT]
// EXISTS. It is empty when the statement names no table there.
func tableAfter(statement Statement, position int) string {
	words, source := statement.Words, statement.SourceWords
	switch {
	case hasPrefix(words[position:], "IF", "NOT", "EXISTS"):
		position += 3
	case hasPrefix(words[position:], "IF", "EXISTS"):
		position += 2
	}
	if position >= len(words) || !name(words[position]) {
		return ""
	}
	if position+2 < len(words) && words[position+1] == "." && name(words[position+2]) {
		return source[position] + "." + source[position+2]
	}
	return source[position]
}

// concurrentIndex is the requirement of a CREATE [UNIQUE] INDEX CONCURRENTLY,
// named by the index and its table as written.
func concurrentIndex(statement Statement) (Effect, bool) {
	words, source := statement.Words, statement.SourceWords
	position := 1
	if hasPrefix(words[position:], "UNIQUE") {
		position++
	}
	if !hasPrefix(words[position:], "INDEX", "CONCURRENTLY") {
		return Effect{}, false
	}
	position += 2
	effect := Effect{Fact: protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY, Capability: createIndexConcurrently, Kind: "CREATE INDEX", Clause: "CONCURRENTLY"}
	if hasPrefix(words[position:], "IF", "NOT", "EXISTS") {
		position += 3
	}
	if position < len(words) && name(words[position]) && words[position] != "ON" {
		effect.Object = source[position]
	}
	if on := slices.Index(words, "ON"); on >= 0 {
		effect.Table = tableAfter(statement, on+1)
	}
	return effect, true
}

// hasPrefix reports whether words start with prefix.
func hasPrefix(words []string, prefix ...string) bool {
	return len(words) >= len(prefix) && slices.Equal(words[:len(prefix)], prefix)
}

// hasSequence reports whether sequence occurs in words.
func hasSequence(words []string, sequence ...string) bool {
	for position := range words {
		if hasPrefix(words[position:], sequence...) {
			return true
		}
	}
	return false
}

// name reports a word that can name a table, a column or an index: a quoted
// name or a bare word.
func name(word string) bool { return strings.HasPrefix(word, `"`) || bare(word) }

// bare reports an unquoted word that starts with a letter or an underscore.
func bare(word string) bool {
	return word != "" && (word[0] == '_' || word[0] >= 'A' && word[0] <= 'Z' || word[0] >= 'a' && word[0] <= 'z')
}
