package devclean

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// Replay judges one statement a migration replay runs on a dev database. It
// returns the empty string for a statement whose effects stay in the dev
// database, where a cleanup removes them, and otherwise the operation it
// refuses, in one line that quotes nothing from the statement.
//
// The judgment accepts what it knows and refuses the rest. A table, an
// index, a view, a sequence, a change stream, a locality group and a schema
// are the objects a cleanup removes, and a row lives in a table, so creating, changing and
// dropping them and reading and writing rows is accepted. A database option,
// a role and a privilege outlive a cleanup, and a session setting or a
// transaction left open reaches the statements the run sends after it, so
// they are refused, as Ptah's built-in replay guard refuses them on the
// PostgreSQL family. A statement the provider cannot read is refused too.
func Replay(statement string) string {
	_, words, err := sqltext.Words(statement)
	switch {
	case err != nil:
		return "a statement the provider cannot read"
	case len(words) == 0:
		return "an empty statement"
	}
	first := words[0]
	second := ""
	if len(words) > 1 {
		second = words[1]
	}
	switch first {
	case "SELECT", "WITH", "INSERT", "UPDATE", "DELETE", "SHOW", "EXPLAIN", "ANALYZE", "VALUES":
		return ""
	case "CREATE", "ALTER", "DROP":
		if confinedObject(words[1:]) {
			return ""
		}
		if second == "" {
			return first + " statement"
		}
		return first + " " + second
	case "GRANT", "REVOKE":
		return first
	case "ABORT", "BEGIN", "CLOSE", "COMMIT", "DEALLOCATE", "DECLARE", "DISCARD", "END", "EXECUTE", "FETCH",
		"LISTEN", "LOCK", "NOTIFY", "PREPARE", "RELEASE", "RESET", "ROLLBACK", "SAVEPOINT", "SET", "START", "UNLISTEN":
		return first + " session or transaction state"
	}
	return first + " statement"
}

// confinedObject reports the object words name after CREATE, ALTER or DROP
// is one a cleanup removes: a table, an index of either kind, a view, a
// sequence, a change stream, a locality group or a schema. ALTER DATABASE, a
// role and anything else are not.
func confinedObject(words []string) bool {
	for len(words) > 0 && slices.Contains([]string{"OR", "REPLACE", "UNIQUE"}, words[0]) {
		words = words[1:]
	}
	if len(words) == 0 {
		return false
	}
	head := strings.Join(words[:min(2, len(words))], " ")
	return slices.Contains([]string{"TABLE", "INDEX", "VIEW", "SEQUENCE", "SCHEMA"}, words[0]) ||
		slices.Contains([]string{"SEARCH INDEX", "CHANGE STREAM", "LOCALITY GROUP"}, head)
}
