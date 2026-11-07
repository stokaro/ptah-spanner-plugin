// Package checkexpr decides whether a declared CHECK expression and the
// clause Spanner keeps for a CHECK are one constraint.
//
// Spanner does not keep a CHECK as it was written. Its PostgreSQL interface
// keeps the parsed tree and prints it back with every implicit cast spelled
// out, every comparison parenthesized and some forms rewritten. Measured on
// the emulator behind PGAdapter 0.56.2:
//
//	score >= 0                   (score >= '0'::bigint)
//	price > 0  (double precision) (price > ('0'::bigint)::double precision)
//	score >= 0.5                 ((score)::numeric >= 0.5)
//	status IN ('a', 'b')         (status = ANY (ARRAY['a'::text, 'b'::text]))
//	status NOT IN ('x')          (status <> 'x'::text)
//	score BETWEEN 1 AND 10       ((score >= '1'::bigint) AND (score <= '10'::bigint))
//	name LIKE 'a%'               (name ~~ 'a%'::text)
//	d > '2020-01-01'  (date)     (d > '2020-01-01'::date)
//	Score >= 0                   (score >= '0'::bigint)
//
// Ptah's built-in Spanner engine compared the two texts by PostgreSQL's rules
// and never matched the first form, so it dropped and added such a CHECK on
// every run (stokaro/ptah#4204 S1). This package reads both sides into the
// same tree instead: it types each literal and column, adds the casts the
// server adds, folds a cast literal into a typed one, rewrites IN, BETWEEN and
// LIKE into the forms the server keeps, and compares the trees. It runs
// offline and reads nothing but the two texts and the table's column types.
//
// Where it cannot read a side it answers that the two differ: the cost of a
// wrong "differ" is a CHECK dropped and added again, and the cost of a wrong
// "equal" is a declared change that never reaches the database. A timestamp
// literal without a time zone is one such side: the server converts it in its
// own time zone, which the expression does not name.
package checkexpr

import (
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
)

// Equivalent reports whether the declared CHECK expression and the clause a
// held CHECK keeps are one constraint on Spanner. columns maps each column of
// the table to its type, in any spelling coltype reads; a column it does not
// name has no known type. An expression either side cannot read is not
// equivalent.
func Equivalent(declared, held string, columns map[string]string) bool {
	left, ok := canonical(declared, columns)
	if !ok {
		return false
	}
	right, ok := canonical(held, columns)
	return ok && left == right
}

// canonical reads an expression and prints its normalized tree.
func canonical(expression string, columns map[string]string) (string, bool) {
	tree, err := parse(expression)
	if err != nil {
		return "", false
	}
	n := normalizer{columns: make(map[string]string, len(columns))}
	for name, columnType := range columns {
		n.columns[name] = typeClass(columnType)
	}
	normalized, ok := n.normalize(tree)
	if !ok {
		return "", false
	}
	var b strings.Builder
	write(&b, normalized)
	return b.String(), true
}

// The numeric classes and their rank: an operation on two of them casts the
// lower to the higher, as PostgreSQL resolves its operators.
var numericRank = map[string]int{classInt8: 1, classNumeric: 2, "double precision": 3, "real": 3}

// typeClass is the class of a type for typing: its canonical spelling without
// a length, so `varchar(10)` and `text` are one class, or the empty string for
// a type coltype does not read, which takes no implicit cast.
func typeClass(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	canonicalType, err := coltype.Canonical(name)
	if err != nil {
		return ""
	}
	if base, _, found := strings.Cut(canonicalType, "("); found {
		canonicalType = base
	}
	if strings.HasSuffix(canonicalType, "[]") {
		return ""
	}
	return canonicalType
}

const (
	classText        = "character varying"
	classFloat8      = "double precision"
	classFloat4      = "real"
	classTimestamptz = "timestamp with time zone"
	classDate        = "date"
)

// comparisons are the operators whose result is a boolean.
var comparisons = []string{"=", "<>", "<", ">", "<=", ">=", "~~", "!~~", "~~*", "!~~*", "~", "!~", "~*", "!~*"}

// arithmetic are the operators whose result has their operands' common type.
var arithmetic = []string{"+", "-", "*", "/", "%"}

// functionClasses are the result classes of the functions a CHECK commonly
// calls. Another function's result has no known class.
var functionClasses = map[string]string{
	"length": classInt8, "char_length": classInt8, "character_length": classInt8, "octet_length": classInt8, "strpos": classInt8,
	"lower": classText, "upper": classText, "btrim": classText, "ltrim": classText, "rtrim": classText, "substr": classText,
	"replace": classText, "concat": classText, "left": classText, "right": classText, "reverse": classText, "lpad": classText, "rpad": classText,
}

// normalizer rewrites a tree into the form the server keeps it in.
type normalizer struct {
	columns map[string]string
}

// normalize returns the tree with each node's class set, the server's implicit
// casts added and its rewrites applied.
func (n normalizer) normalize(tree *node) (*node, bool) {
	switch tree.kind {
	case nodeColumn:
		return &node{kind: nodeColumn, text: tree.text, class: n.columns[tree.text]}, true
	case nodeLiteral:
		return n.literal(tree)
	case nodeCast:
		inner, ok := n.normalize(tree.args[0])
		if !ok {
			return nil, false
		}
		return castTo(inner, typeClass(tree.op), tree.op), true
	case nodeUnary:
		inner, ok := n.normalize(tree.args[0])
		if !ok {
			return nil, false
		}
		if inner.kind == nodeLiteral && numericRank[inner.class] > 0 {
			value, ok := numberText("-" + inner.text)
			return &node{kind: nodeLiteral, text: value, class: inner.class}, ok
		}
		return &node{kind: nodeUnary, op: tree.op, args: []*node{inner}, class: inner.class}, true
	case nodeBinary:
		return n.binary(tree.op, tree.args[0], tree.args[1])
	case nodeAnd, nodeOr:
		return n.logical(tree)
	case nodeNot, nodeIsNull:
		inner, ok := n.normalize(tree.args[0])
		return &node{kind: tree.kind, args: []*node{inner}, not: tree.not, class: classBool}, ok
	case nodeBetween:
		return n.between(tree)
	case nodeIn:
		return n.in(tree.args[0], tree.args[1:], inOperators[tree.not])
	case nodeAnyAll:
		return n.anyAll(tree)
	case nodeCall:
		return n.call(tree)
	case nodeArray:
		items, ok := n.all(tree.args)
		return &node{kind: nodeArray, args: items}, ok
	}
	return nil, false
}

func (n normalizer) all(trees []*node) ([]*node, bool) {
	result := make([]*node, len(trees))
	for i, tree := range trees {
		normalized, ok := n.normalize(tree)
		if !ok {
			return nil, false
		}
		result[i] = normalized
	}
	return result, true
}

// literal gives a number its canonical value.
func (normalizer) literal(tree *node) (*node, bool) {
	if numericRank[tree.class] == 0 {
		return &node{kind: nodeLiteral, text: tree.text, class: tree.class}, true
	}
	value, ok := numberText(tree.text)
	return &node{kind: nodeLiteral, text: value, class: tree.class}, ok
}

// binary types an operation and casts its operands where the server does.
func (n normalizer) binary(op string, leftTree, rightTree *node) (*node, bool) {
	left, ok := n.normalize(leftTree)
	if !ok {
		return nil, false
	}
	right, ok := n.normalize(rightTree)
	if !ok {
		return nil, false
	}
	var common string
	left, right, common = unify(left, right)
	result := &node{kind: nodeBinary, op: op, args: []*node{left, right}}
	switch {
	case slices.Contains(comparisons, op):
		result.class = classBool
	case slices.Contains(arithmetic, op):
		result.class = common
	case op == "||":
		result.class = classText
	}
	return result, true
}

// unify casts the operands of an operation to their common class, as
// PostgreSQL resolves the operator: a string literal takes the other side's
// type, and of two numeric classes the lower is cast to the higher. real and
// double precision meet without a cast, and an integer or numeric beside real
// is cast to double precision.
func unify(left, right *node) (unifiedLeft, unifiedRight *node, common string) {
	switch {
	case left.class == classUnknown && right.class == classUnknown:
		return castTo(left, classText, classText), castTo(right, classText, classText), classText
	case left.class == classUnknown && right.class != "":
		return castTo(left, right.class, right.class), right, right.class
	case right.class == classUnknown && left.class != "":
		return left, castTo(right, left.class, left.class), left.class
	}
	leftRank, rightRank := numericRank[left.class], numericRank[right.class]
	if leftRank == 0 || rightRank == 0 || left.class == right.class {
		return left, right, left.class
	}
	if leftRank == 3 && rightRank == 3 {
		return left, right, classFloat8
	}
	target := left.class
	if rightRank > leftRank {
		target = right.class
	}
	if target == classFloat4 {
		target = classFloat8
	}
	if left.class != target && left.class != classFloat4 {
		left = castTo(left, target, target)
	}
	if right.class != target && right.class != classFloat4 {
		right = castTo(right, target, target)
	}
	return left, right, target
}

// castTo casts a node to a class. A literal becomes a literal of the class,
// and a cast to the class the node already has is no cast, as the server
// prints none. spelled is the type as written, which a cast to a class
// typeClass does not read keeps.
func castTo(operand *node, class, spelled string) *node {
	if class == "" {
		class = "?" + strings.ToLower(strings.TrimSpace(spelled))
	}
	if operand.class == class {
		return operand
	}
	if operand.kind == nodeLiteral && operand.class != classNull {
		return &node{kind: nodeLiteral, text: literalValue(operand.text, class), class: class}
	}
	return &node{kind: nodeCast, op: class, args: []*node{operand}, class: class}
}

// literalValue is a literal's value in a class: a number in canonical form, a
// boolean as true or false, a time with a zone as its instant, and any other
// value as written.
func literalValue(text, class string) string {
	switch {
	case numericRank[class] > 0:
		if value, ok := numberText(text); ok {
			return value
		}
	case class == classBool:
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "t", "true", "y", "yes", "on", "1":
			return "true"
		case "f", "false", "n", "no", "off", "0":
			return "false"
		}
	case class == classTimestamptz:
		if instant, ok := zonedInstant(text); ok {
			return instant
		}
	case class == classDate:
		return strings.TrimSpace(text)
	}
	return text
}

// numberText is a number's canonical value: 0.50, .5 and 5e-1 are one value.
func numberText(text string) (string, bool) {
	value, ok := new(big.Rat).SetString(strings.TrimSpace(text))
	if !ok {
		return "", false
	}
	return value.RatString(), true
}

// zonedLayouts are the spellings of a time with a zone the server prints and
// a declaration commonly writes.
var zonedLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999Z07", "2006-01-02T15:04:05.999999999Z07"}

// zonedInstant reads a time that names its zone as its instant in UTC.
func zonedInstant(text string) (string, bool) {
	for _, layout := range zonedLayouts {
		if instant, err := time.Parse(layout, strings.TrimSpace(text)); err == nil {
			return instant.UTC().Format(time.RFC3339Nano), true
		}
	}
	return "", false
}

// logical flattens nested AND or OR into one list, as the server keeps a run
// of them.
func (n normalizer) logical(tree *node) (*node, bool) {
	result := &node{kind: tree.kind, class: classBool}
	for _, arg := range tree.args {
		normalized, ok := n.normalize(arg)
		if !ok {
			return nil, false
		}
		if normalized.kind == tree.kind {
			result.args = append(result.args, normalized.args...)
			continue
		}
		result.args = append(result.args, normalized)
	}
	return result, true
}

// between rewrites BETWEEN as the server keeps it: two comparisons joined by
// AND, or for NOT BETWEEN joined by OR.
func (n normalizer) between(tree *node) (*node, bool) {
	operand, low, high := tree.args[0], tree.args[1], tree.args[2]
	if tree.not {
		return n.logical(&node{kind: nodeOr, args: []*node{
			{kind: nodeBinary, op: "<", args: []*node{operand, low}}, {kind: nodeBinary, op: ">", args: []*node{operand, high}},
		}})
	}
	return n.logical(&node{kind: nodeAnd, args: []*node{
		{kind: nodeBinary, op: ">=", args: []*node{operand, low}}, {kind: nodeBinary, op: "<=", args: []*node{operand, high}},
	}})
}

// inOperators are the comparisons IN stands for, = for IN and <> for NOT IN.
var inOperators = map[bool]string{false: "=", true: "<>"}

// in types an IN list whose comparison is op, = for IN and <> for NOT IN. A
// list of one item is the comparison the server keeps for it; a longer one
// stays a list, which the server keeps as = ANY or <> ALL of an array.
func (n normalizer) in(operandTree *node, itemTrees []*node, op string) (*node, bool) {
	if len(itemTrees) == 1 {
		return n.binary(op, operandTree, itemTrees[0])
	}
	operand, ok := n.normalize(operandTree)
	if !ok {
		return nil, false
	}
	items, ok := n.all(itemTrees)
	if !ok {
		return nil, false
	}
	for _, item := range items {
		operand, _, _ = unify(operand, item)
	}
	for i := range items {
		_, items[i], _ = unify(operand, items[i])
	}
	return &node{kind: nodeIn, args: append([]*node{operand}, items...), not: op == "<>", class: classBool}, true
}

// anyAll reads = ANY and <> ALL of an array as the IN list they stand for.
// Any other operator keeps its form.
func (n normalizer) anyAll(tree *node) (*node, bool) {
	operand, array := tree.args[0], tree.args[1]
	if array.kind == nodeArray && (tree.op == "=" && !tree.not || tree.op == "<>" && tree.not) {
		return n.in(operand, array.args, tree.op)
	}
	args, ok := n.all(tree.args)
	return &node{kind: nodeAnyAll, op: tree.op, args: args, not: tree.not, class: classBool}, ok
}

// call types a function call by its result. A string literal argument is
// text, as the server spells it.
func (n normalizer) call(tree *node) (*node, bool) {
	args, ok := n.all(tree.args)
	if !ok {
		return nil, false
	}
	result := &node{kind: nodeCall, op: tree.op, args: args, class: functionClasses[tree.op]}
	if tree.op == "abs" && len(args) == 1 {
		result.class = args[0].class
	}
	return result, true
}

// write prints a normalized tree in one fixed form. A string literal that met
// no type is text, as the server types it.
func write(b *strings.Builder, tree *node) {
	switch tree.kind {
	case nodeColumn:
		b.WriteString(`"` + tree.text + `"`)
	case nodeLiteral:
		class := tree.class
		if class == classUnknown {
			class = classText
		}
		b.WriteString(class + ":'" + strings.ReplaceAll(tree.text, "'", "''") + "'")
	case nodeCast:
		b.WriteString("cast(")
		write(b, tree.args[0])
		b.WriteString(" as " + tree.op + ")")
	case nodeUnary:
		b.WriteString("(" + tree.op)
		write(b, tree.args[0])
		b.WriteString(")")
	case nodeBinary:
		b.WriteString("(")
		write(b, tree.args[0])
		b.WriteString(" " + tree.op + " ")
		write(b, tree.args[1])
		b.WriteString(")")
	case nodeAnd, nodeOr:
		joiner := " and "
		if tree.kind == nodeOr {
			joiner = " or "
		}
		writeList(b, "(", tree.args, joiner, ")")
	case nodeNot:
		b.WriteString("(not ")
		write(b, tree.args[0])
		b.WriteString(")")
	case nodeIsNull:
		b.WriteString("(")
		write(b, tree.args[0])
		b.WriteString(map[bool]string{false: " is null)", true: " is not null)"}[tree.not])
	case nodeIn:
		b.WriteString("(")
		write(b, tree.args[0])
		b.WriteString(map[bool]string{false: " in ", true: " not in "}[tree.not])
		writeList(b, "(", tree.args[1:], ", ", "))")
	case nodeAnyAll:
		b.WriteString("(")
		write(b, tree.args[0])
		b.WriteString(" " + tree.op + map[bool]string{false: " any ", true: " all "}[tree.not])
		write(b, tree.args[1])
		b.WriteString(")")
	case nodeCall:
		writeList(b, tree.op+"(", tree.args, ", ", ")")
	case nodeArray:
		writeList(b, "array[", tree.args, ", ", "]")
	}
}

func writeList(b *strings.Builder, open string, items []*node, joiner, closing string) {
	b.WriteString(open)
	for i, item := range items {
		if i > 0 {
			b.WriteString(joiner)
		}
		write(b, item)
	}
	b.WriteString(closing)
}
