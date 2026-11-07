package migrationlint

import (
	"context"
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
)

// File is one migration file of a check, with the statements the host split
// it into through this package. The rules judge up and down files alike, so
// a file is its statements.
type File struct {
	Statements []Statement
}

// Finding is one violation of a rule: the file and statement it is on, by
// position in the request, and the message.
type Finding struct {
	Code      string
	File      int
	Statement int
	Message   string
}

// rule is one rule the provider publishes and the function that checks one
// statement. A rule judges every file of a run, up and down alike, and says
// so in AppliesToDown.
type rule struct {
	Code     string
	Title    string
	Severity protocol.LintRule_Severity
	// Subsumes names the host's rules whose finding on the same statement
	// this rule's finding replaces.
	Subsumes  []string
	Statement func(Statement) (bool, string)
}

// setNotNullMessage is what Spanner does with SET NOT NULL, in the words the
// host's PostgreSQL owner reported under PG303 for Spanner while Spanner was
// built in. Measured on the emulator behind PGAdapter: a row holding NULL
// fails the schema change with `FAILED_PRECONDITION: Cannot specify a null
// value for column: a`, and the column stays nullable.
const setNotNullMessage = "SET NOT NULL runs as a schema change that validates every existing row, and a row holding NULL fails it " +
	"(FAILED_PRECONDITION: Cannot specify a null value for column) and leaves the column nullable; backfill first"

// dropTTLMessage says what ALTER TABLE ... DROP TTL does. The host's DS102 and
// BC104 read `DROP <word>` as a column drop, so a TTL drop read as a DROP
// COLUMN of a column named TTL, which Spanner reserves as a word: a column of
// that name has to be quoted, and the measured statement keeps every column.
const dropTTLMessage = "DROP TTL removes the table's row deletion policy and drops no column; rows the policy would have deleted " +
	"are kept from now on"

// catalog is every rule the provider publishes, in code order.
//
// SP101 is the check the host's PostgreSQL owner made for Spanner as PG303
// while Spanner was built in. The provider cannot publish PG303 itself: a run
// whose runtime holds the built-in engines beside this provider would hold two
// rules of one code, which the host refuses when it builds the runtime.
func catalog() []rule {
	return []rule{
		{
			Code: "SP101", Title: "not-null validation scans existing rows", Severity: protocol.LintRule_WARNING,
			Statement: func(statement Statement) (bool, string) { return setsNotNull(statement.Words), setNotNullMessage },
		},
		{
			Code: "SP102", Title: "row deletion policy dropped", Severity: protocol.LintRule_INFO,
			Subsumes:  []string{"BC104", "DS102"},
			Statement: func(statement Statement) (bool, string) { return dropsTTL(statement.Words), dropTTLMessage },
		},
	}
}

// Rules returns the rule catalog the provider publishes, in code order. Each
// call returns fresh values.
func Rules() []*protocol.LintRule {
	all := catalog()
	result := make([]*protocol.LintRule, 0, len(all))
	for _, rule := range all {
		result = append(result, &protocol.LintRule{Code: rule.Code, Title: rule.Title, Severity: rule.Severity,
			AppliesToDown: true, Subsumes: slices.Clone(rule.Subsumes)})
	}
	return result
}

// Check runs the rules codes enables over files and returns their findings:
// for each file in order, each code's findings in the order codes lists them,
// in statement order. A code the provider does not publish is refused, and
// cancellation returns the context's error; either returns no finding.
func Check(ctx context.Context, codes []string, files []File) ([]Finding, error) {
	all := catalog()
	enabled := make([]rule, 0, len(codes))
	for _, code := range codes {
		position := slices.IndexFunc(all, func(rule rule) bool { return rule.Code == code })
		if position < 0 {
			return nil, fmt.Errorf("the provider publishes no lint rule %s", code)
		}
		enabled = append(enabled, all[position])
	}
	var findings []Finding
	for index, file := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, rule := range enabled {
			for position, statement := range file.Statements {
				if found, message := rule.Statement(statement); found {
					findings = append(findings, Finding{Code: rule.Code, File: index, Statement: position, Message: message})
				}
			}
		}
	}
	return findings, ctx.Err()
}

// setsNotNull reports an ALTER TABLE with a clause that sets a column NOT
// NULL: ALTER [COLUMN] name ... SET NOT NULL, as the host's PG303 read one.
func setsNotNull(words []string) bool {
	if !hasPrefix(words, "ALTER", "TABLE") {
		return false
	}
	for _, clause := range clauses(words) {
		if !hasPrefix(clause, "ALTER") || !hasSequence(clause, "SET", "NOT", "NULL") {
			continue
		}
		if hasPrefix(clause, "ALTER", "COLUMN") || len(clause) > 1 && name(clause[1]) {
			return true
		}
	}
	return false
}

// dropsTTL reports an ALTER TABLE with a DROP TTL clause, the unquoted word:
// a quoted "TTL" names a column.
func dropsTTL(words []string) bool {
	if !hasPrefix(words, "ALTER", "TABLE") {
		return false
	}
	return slices.ContainsFunc(clauses(words), func(clause []string) bool {
		return len(clause) == 2 && clause[0] == "DROP" && clause[1] == "TTL"
	})
}

// clauses are the clauses of an ALTER TABLE: the words after the table's
// name, cut at each comma outside parentheses.
func clauses(words []string) [][]string {
	position := 2
	if hasPrefix(words[position:], "IF", "EXISTS") {
		position += 2
	}
	if hasPrefix(words[position:], "ONLY") {
		position++
	}
	if position < len(words) && name(words[position]) {
		position++
		if position+1 < len(words) && words[position] == "." && name(words[position+1]) {
			position += 2
		}
	}
	var result [][]string
	depth, start := 0, position
	for index := position; index < len(words); index++ {
		switch words[index] {
		case "(":
			depth++
		case ")":
			depth = max(depth-1, 0)
		case ",":
			if depth == 0 {
				result = append(result, words[start:index])
				start = index + 1
			}
		}
	}
	return append(result, words[start:])
}
