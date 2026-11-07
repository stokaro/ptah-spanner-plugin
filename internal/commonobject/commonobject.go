// Package commonobject carries the Spanner objects beyond tables that travel
// on Ptah's common provider wire: schemas, views and sequences. The host
// compares and orders them for every engine; this package says what Spanner
// holds of each, gives a declaration in Spanner's spelling, and writes the
// statement each change is made with, as Ptah's built-in Spanner engine wrote
// it.
//
// Spanner's PostgreSQL interface stores no comment and has no role, grant or
// materialized view, so the engine carries none of those, and the host
// refuses a declaration that writes one by name.
package commonobject

import (
	"fmt"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// Support is the declaration of the kinds the spanner engine carries. A host
// that does not negotiate schema.common-namespaces receives it without
// SCHEMA.
func Support() *protocol.CommonObjectSupport {
	return &protocol.CommonObjectSupport{Kinds: []protocol.CommonObjectSupport_Kind{
		protocol.CommonObjectSupport_VIEW, protocol.CommonObjectSupport_SEQUENCE, protocol.CommonObjectSupport_SCHEMA,
	}}
}

// Capabilities are the capability facts a session reports beside its own, as
// Ptah's built-in Spanner engine measured them on the emulator behind
// PGAdapter: views, replaced in place by CREATE OR REPLACE VIEW, with no
// option a read would report back (WITH (security_invoker = true) is taken
// and reads back as nothing); no materialized view; sequences whose only
// option is START COUNTER WITH (START WITH, INCREMENT BY and AS answer
// `Optional clause <start> is not supported`).
func Capabilities() map[string]bool {
	return map[string]bool{
		"views": true, "create_or_replace_view": true, "view_options": false, "materialized_views": false,
		"sequences": true, "sequence_start_counter_only": true,
	}
}

// Prepare returns a declaration in Spanner's spelling: each view's query as
// ViewQuery writes it, and each sequence's type as the read reports it. No
// entry changes its identity.
func Prepare(declared *protocol.DeclaredCommonObjects) *protocol.DeclaredCommonObjects {
	prepared := proto.CloneOf(declared)
	if prepared == nil {
		return &protocol.DeclaredCommonObjects{}
	}
	for _, view := range prepared.GetViews() {
		view.Body = ViewQuery(view.GetBody())
	}
	for _, sequence := range prepared.GetSequences() {
		if bigint(sequence.GetAsType()) {
			sequence.AsType = SequenceType
		}
	}
	return prepared
}

// SequenceType is the type a read reports for every sequence: Spanner's
// catalog says INT64, which is the 64-bit integer the PostgreSQL interface
// calls bigint.
const SequenceType = "bigint"

// bigint reports a sequence type that is Spanner's one integer type.
func bigint(declared string) bool {
	switch strings.ToLower(strings.TrimSpace(declared)) {
	case "bigint", "int8", "int64":
		return true
	}
	return false
}

// Refusal is a declared object Spanner cannot hold: its kind, its name, and
// why.
type Refusal struct {
	Kind, Object, Reason string
}

// Refusals names what Spanner cannot hold in a declaration, one refusal per
// object, views before sequences, each in declaration order.
func Refusals(declared *protocol.DeclaredCommonObjects) []Refusal {
	var refusals []Refusal
	for _, view := range declared.GetViews() {
		if reason := viewRefusal(view); reason != "" {
			refusals = append(refusals, Refusal{Kind: "view", Object: ddl.Name(view.GetSchema(), view.GetName()), Reason: reason})
		}
	}
	for _, sequence := range declared.GetSequences() {
		if reason := sequenceRefusal(sequence); reason != "" {
			refusals = append(refusals, Refusal{Kind: "sequence", Object: ddl.Name(sequence.GetSchema(), sequence.GetName()), Reason: reason})
		}
	}
	return refusals
}

func viewRefusal(view *protocol.SQLView) string {
	subject := "view " + ddl.Name(view.GetSchema(), view.GetName())
	switch {
	case view.GetMaterialized():
		return subject + ": Spanner has no materialized view"
	case strings.TrimSpace(view.GetBody()) == "":
		return subject + ": a view needs a query"
	case view.GetWithCheck():
		return "WITH CHECK OPTION on " + subject + ": Spanner's views cannot be written through"
	case len(view.GetAttributes()) > 0:
		return fmt.Sprintf("WITH (%s) on %s: a Spanner view keeps no option a read would report back", strings.Join(view.GetAttributes(), ", "), subject)
	case view.GetComment() != "":
		return "the comment on " + subject + ": Spanner's PostgreSQL interface stores no comment"
	}
	return ""
}

func sequenceRefusal(sequence *protocol.SQLSequence) string {
	subject := "sequence " + ddl.Name(sequence.GetSchema(), sequence.GetName())
	switch {
	case sequence.GetAsType() != "" && !bigint(sequence.GetAsType()):
		return fmt.Sprintf("%s is declared AS %s: a Spanner sequence is a bigint", subject, sequence.GetAsType())
	case sequence.Increment != nil || sequence.MinValue != nil || sequence.MaxValue != nil || sequence.Cache != nil || sequence.GetCycle():
		return subject + " declares INCREMENT, MINVALUE, MAXVALUE, CACHE or CYCLE: a Spanner sequence is bit-reversed and takes START COUNTER WITH alone"
	case sequence.GetOwnedBy() != "":
		return subject + " is OWNED BY a column: Spanner keeps no owner of a sequence"
	case sequence.GetComment() != "":
		return "the comment on " + subject + ": Spanner's PostgreSQL interface stores no comment"
	}
	return ""
}

// Render writes the statement of one change, terminated, as Ptah's built-in
// Spanner engine wrote it. A change Spanner cannot make is refused with a
// reason that names the object.
func Render(change *protocol.CommonObjectChange) (string, error) {
	switch operation := change.GetOperation().(type) {
	case *protocol.CommonObjectChange_CreateView:
		return createView(operation.CreateView)
	case *protocol.CommonObjectChange_DropView:
		view := operation.DropView
		if view.GetKind() != "view" {
			return "", fmt.Errorf("DROP of %s %s: Spanner has no materialized view", view.GetKind(), ddl.Name(view.GetSchema(), view.GetName()))
		}
		return "DROP VIEW " + ddl.Name(view.GetSchema(), view.GetName()) + ";", nil
	case *protocol.CommonObjectChange_CreateSequence:
		return createSequence(operation.CreateSequence)
	case *protocol.CommonObjectChange_AlterSequence:
		return alterSequence(operation.AlterSequence)
	case *protocol.CommonObjectChange_DropSequence:
		sequence := operation.DropSequence
		return "DROP SEQUENCE " + ddl.Name(sequence.GetSchema(), sequence.GetName()) + ";", nil
	case *protocol.CommonObjectChange_CreateSchema:
		return createSchema(operation.CreateSchema)
	case *protocol.CommonObjectChange_DropSchema:
		// Spanner drops an empty schema only (`Cannot drop schema app while
		// it has dependencies`) and takes no CASCADE, which the host never
		// asks for.
		return "DROP SCHEMA " + ddl.Quote(operation.DropSchema.GetName()) + ";", nil
	default:
		return "", fmt.Errorf("the provider writes schemas, views and sequences, and Spanner has no %s", changeNoun(change))
	}
}

// createSchema writes CREATE SCHEMA IF NOT EXISTS, the statement Ptah's
// built-in Spanner engine wrote for a declared schema and for the schema of a
// table it created. Spanner's PostgreSQL interface stores no comment, so a
// schema that carries one is refused.
func createSchema(creation *protocol.NamespaceCreation) (string, error) {
	if creation.GetComment() != "" {
		return "", fmt.Errorf("the comment on schema %s: Spanner's PostgreSQL interface stores no comment", ddl.Quote(creation.GetName()))
	}
	return ddl.CreateSchema(creation.GetName()), nil
}

// createView writes CREATE VIEW, or CREATE OR REPLACE VIEW for a replacement,
// with the query on a line of its own as the built-in engine wrote it. It
// writes no SQL SECURITY clause, as the built-in engine wrote none: the
// emulator takes a view without it and reads every view back as INVOKER, and
// Ptah's SQL reader does not parse the clause, so an export that wrote it
// could not be read back.
func createView(creation *protocol.ViewCreation) (string, error) {
	view := creation.GetView()
	if reason := viewRefusal(view); reason != "" {
		return "", fmt.Errorf("%s", reason)
	}
	verb := "CREATE VIEW "
	if creation.GetReplace() {
		verb = "CREATE OR REPLACE VIEW "
	}
	return verb + ddl.Name(view.GetSchema(), view.GetName()) + " AS\n" + strings.TrimRight(strings.TrimSpace(view.GetBody()), ";") + ";", nil
}

// createSequence writes CREATE SEQUENCE with the counter start the
// declaration states. Spanner's default kind is bit_reversed_positive, and
// the statement names no kind, as the built-in engine wrote it.
func createSequence(sequence *protocol.SQLSequence) (string, error) {
	if reason := sequenceRefusal(sequence); reason != "" {
		return "", fmt.Errorf("%s", reason)
	}
	statement := "CREATE SEQUENCE " + ddl.Name(sequence.GetSchema(), sequence.GetName())
	if sequence.Start != nil {
		statement += " START COUNTER WITH " + strconv.FormatInt(sequence.GetStart(), 10)
	}
	return statement + ";", nil
}

// alterSequence writes RESTART COUNTER WITH for a changed start, which is
// the only change Spanner makes to a sequence.
func alterSequence(alteration *protocol.SequenceAlteration) (string, error) {
	desired := alteration.GetDesired()
	subject := "sequence " + ddl.Name(alteration.GetSequence().GetSchema(), alteration.GetSequence().GetName())
	for _, changed := range alteration.GetChanged() {
		if changed != "start" {
			return "", fmt.Errorf("changing the %s of %s: a Spanner sequence takes START COUNTER WITH alone", changed, subject)
		}
	}
	if desired.Start == nil {
		return "", fmt.Errorf("changing %s: the declaration states no counter start to restart at", subject)
	}
	return "ALTER SEQUENCE " + ddl.Name(alteration.GetSequence().GetSchema(), alteration.GetSequence().GetName()) +
		" RESTART COUNTER WITH " + strconv.FormatInt(desired.GetStart(), 10) + ";", nil
}

func changeNoun(change *protocol.CommonObjectChange) string {
	switch change.GetOperation().(type) {
	case *protocol.CommonObjectChange_CreateRole, *protocol.CommonObjectChange_AlterRole, *protocol.CommonObjectChange_DropRole,
		*protocol.CommonObjectChange_AddMember, *protocol.CommonObjectChange_RemoveMember:
		return "role"
	case *protocol.CommonObjectChange_Grant, *protocol.CommonObjectChange_Revoke:
		return "grant"
	case *protocol.CommonObjectChange_SetComment:
		return "comment"
	default:
		return "such object"
	}
}
