package checkexpr

import (
	"fmt"
	"slices"
	"strings"
)

// nodeKind is what a node of an expression is.
type nodeKind int

const (
	nodeColumn nodeKind = iota
	nodeLiteral
	nodeCast
	nodeUnary
	nodeBinary
	nodeAnd
	nodeOr
	nodeNot
	nodeIsNull
	nodeIn
	nodeAnyAll
	nodeBetween
	nodeCall
	nodeArray
)

// node is one node of an expression tree. Each kind reads the fields its
// comment in parse names.
type node struct {
	kind nodeKind
	// op is the operator of a unary, binary or ANY/ALL node, the function of
	// a call, and the target type of a cast.
	op string
	// text is a column's name and a literal's value.
	text string
	// class is a literal's type class, and after normalization the type
	// class of any node; see typeClass.
	class string
	args  []*node
	// not negates an IS NULL, IN or BETWEEN node, and marks an ANY/ALL node
	// as ALL.
	not bool
}

// The literal classes a literal has before it meets a type: an integer, a
// number with a fraction or an exponent, a string, a boolean and NULL.
const (
	classInt8    = "bigint"
	classNumeric = "numeric"
	classUnknown = "unknown"
	classBool    = "boolean"
	classNull    = "null"
)

// parser reads tokens by PostgreSQL's operator precedence.
type parser struct {
	tokens []token
	pos    int
}

// parse reads a whole expression.
func parse(input string) (*node, error) {
	tokens, err := lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	expression, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokenEnd {
		return nil, fmt.Errorf("unexpected %q", p.peek().text)
	}
	return expression, nil
}

func (p *parser) peek() token { return p.tokens[p.pos] }

func (p *parser) next() token {
	t := p.tokens[p.pos]
	if t.kind != tokenEnd {
		p.pos++
	}
	return t
}

// word reports whether the next token is the keyword w, and consumes it if
// so.
func (p *parser) word(w string) bool {
	if t := p.peek(); t.kind == tokenWord && t.text == w {
		p.pos++
		return true
	}
	return false
}

// wordAhead reports whether the token n places ahead is the keyword w.
func (p *parser) wordAhead(n int, w string) bool {
	if p.pos+n >= len(p.tokens) {
		return false
	}
	t := p.tokens[p.pos+n]
	return t.kind == tokenWord && t.text == w
}

func (p *parser) punct(text string) bool {
	if t := p.peek(); t.kind == tokenPunct && t.text == text {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expect(text string) error {
	if !p.punct(text) {
		return fmt.Errorf("expected %q, found %q", text, p.peek().text)
	}
	return nil
}

func (p *parser) operatorIs(ops ...string) (string, bool) {
	if t := p.peek(); t.kind == tokenOperator && slices.Contains(ops, t.text) {
		p.pos++
		return t.text, true
	}
	return "", false
}

func (p *parser) or() (*node, error) {
	left, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.word("or") {
		right, err := p.and()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeOr, args: []*node{left, right}}
	}
	return left, nil
}

func (p *parser) and() (*node, error) {
	left, err := p.not()
	if err != nil {
		return nil, err
	}
	for p.word("and") {
		right, err := p.not()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeAnd, args: []*node{left, right}}
	}
	return left, nil
}

func (p *parser) not() (*node, error) {
	if p.word("not") {
		operand, err := p.not()
		if err != nil {
			return nil, err
		}
		return &node{kind: nodeNot, args: []*node{operand}}, nil
	}
	return p.is()
}

// is reads IS [NOT] NULL after a comparison. IS TRUE, IS DISTINCT FROM and
// the other IS forms are refused.
func (p *parser) is() (*node, error) {
	left, err := p.comparison()
	if err != nil {
		return nil, err
	}
	for p.word("is") {
		negated := p.word("not")
		if !p.word("null") {
			return nil, fmt.Errorf("an IS form other than IS NULL")
		}
		left = &node{kind: nodeIsNull, args: []*node{left}, not: negated}
	}
	return left, nil
}

// comparison reads a comparison, with ANY or ALL on its right.
func (p *parser) comparison() (*node, error) {
	left, err := p.predicate()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.operatorIs("=", "<>", "!=", "<", ">", "<=", ">=")
		if !ok {
			return left, nil
		}
		if op == "!=" {
			op = "<>"
		}
		if (p.wordAhead(0, "any") || p.wordAhead(0, "some") || p.wordAhead(0, "all")) && p.pos+1 < len(p.tokens) &&
			p.tokens[p.pos+1].kind == tokenPunct && p.tokens[p.pos+1].text == "(" {
			all := p.next().text == "all"
			p.next()
			operand, err := p.or()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			left = &node{kind: nodeAnyAll, op: op, args: []*node{left, operand}, not: all}
			continue
		}
		right, err := p.predicate()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeBinary, op: op, args: []*node{left, right}}
	}
}

// predicate reads BETWEEN, IN, LIKE and ILIKE, each optionally negated.
func (p *parser) predicate() (*node, error) {
	left, err := p.other()
	if err != nil {
		return nil, err
	}
	negated := false
	if p.wordAhead(0, "not") && (p.wordAhead(1, "between") || p.wordAhead(1, "in") || p.wordAhead(1, "like") || p.wordAhead(1, "ilike")) {
		p.next()
		negated = true
	}
	switch {
	case p.word("between"):
		low, err := p.other()
		if err != nil {
			return nil, err
		}
		if !p.word("and") {
			return nil, fmt.Errorf("a BETWEEN without AND")
		}
		high, err := p.other()
		if err != nil {
			return nil, err
		}
		return &node{kind: nodeBetween, args: []*node{left, low, high}, not: negated}, nil
	case p.word("in"):
		if err := p.expect("("); err != nil {
			return nil, err
		}
		items, err := p.list(")")
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("an empty IN list")
		}
		return &node{kind: nodeIn, args: append([]*node{left}, items...), not: negated}, nil
	case p.word("like"):
		return p.pattern(left, negation[negated]+"~~")
	case p.word("ilike"):
		return p.pattern(left, negation[negated]+"~~*")
	}
	return left, nil
}

// negation is what the server puts in front of the operator it keeps for
// LIKE (~~) and ILIKE (~~*) when they are negated.
var negation = map[bool]string{true: "!"}

// pattern reads the right side of LIKE or ILIKE, which the server keeps as
// the operator op.
func (p *parser) pattern(left *node, op string) (*node, error) {
	right, err := p.other()
	if err != nil {
		return nil, err
	}
	if p.word("escape") {
		return nil, fmt.Errorf("a LIKE with ESCAPE")
	}
	return &node{kind: nodeBinary, op: op, args: []*node{left, right}}, nil
}

// other reads any operator that is neither a comparison nor arithmetic, such
// as ~~, ~ and ||, which bind tighter than a comparison.
func (p *parser) other() (*node, error) {
	left, err := p.additive()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t.kind != tokenOperator || slices.Contains([]string{"=", "<>", "!=", "<", ">", "<=", ">=", "+", "-", "*", "/", "%", "^"}, t.text) {
			return left, nil
		}
		p.next()
		right, err := p.additive()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeBinary, op: t.text, args: []*node{left, right}}
	}
}

func (p *parser) additive() (*node, error) {
	return p.binaryLevel(p.multiplicative, "+", "-")
}

func (p *parser) multiplicative() (*node, error) {
	return p.binaryLevel(p.power, "*", "/", "%")
}

func (p *parser) power() (*node, error) {
	return p.binaryLevel(p.unary, "^")
}

func (p *parser) binaryLevel(operand func() (*node, error), ops ...string) (*node, error) {
	left, err := operand()
	if err != nil {
		return nil, err
	}
	for {
		op, ok := p.operatorIs(ops...)
		if !ok {
			return left, nil
		}
		right, err := operand()
		if err != nil {
			return nil, err
		}
		left = &node{kind: nodeBinary, op: op, args: []*node{left, right}}
	}
}

func (p *parser) unary() (*node, error) {
	if op, ok := p.operatorIs("-", "+"); ok {
		operand, err := p.unary()
		if err != nil {
			return nil, err
		}
		if op == "+" {
			return operand, nil
		}
		return &node{kind: nodeUnary, op: op, args: []*node{operand}}, nil
	}
	return p.postfix()
}

// postfix reads a primary and each cast after it. A subscript is refused.
func (p *parser) postfix() (*node, error) {
	operand, err := p.primary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.punct("::"):
			name, err := p.typeName()
			if err != nil {
				return nil, err
			}
			operand = &node{kind: nodeCast, op: name, args: []*node{operand}}
		case p.peek().kind == tokenPunct && p.peek().text == "[":
			return nil, fmt.Errorf("a subscript")
		default:
			return operand, nil
		}
	}
}

func (p *parser) primary() (*node, error) {
	t := p.next()
	switch t.kind {
	case tokenNumber:
		class := classInt8
		if strings.ContainsAny(t.text, ".eE") {
			class = classNumeric
		}
		return &node{kind: nodeLiteral, text: t.text, class: class}, nil
	case tokenString:
		return &node{kind: nodeLiteral, text: t.text, class: classUnknown}, nil
	case tokenName:
		if p.peek().kind == tokenPunct && p.peek().text == "(" {
			return nil, fmt.Errorf("a quoted function name")
		}
		return &node{kind: nodeColumn, text: t.text}, nil
	case tokenPunct:
		if t.text != "(" {
			return nil, fmt.Errorf("unexpected %q", t.text)
		}
		inner, err := p.or()
		if err != nil {
			return nil, err
		}
		return inner, p.expect(")")
	case tokenWord:
		return p.wordPrimary(t.text)
	}
	return nil, fmt.Errorf("an incomplete expression")
}

// wordPrimary reads a primary that starts with the unquoted word: a boolean,
// NULL, an ARRAY, a CAST, a column or a function call.
func (p *parser) wordPrimary(word string) (*node, error) {
	switch word {
	case "true", "false":
		return &node{kind: nodeLiteral, text: word, class: classBool}, nil
	case "null":
		return &node{kind: nodeLiteral, text: "null", class: classNull}, nil
	case "array":
		if err := p.expect("["); err != nil {
			return nil, err
		}
		items, err := p.list("]")
		if err != nil {
			return nil, err
		}
		return &node{kind: nodeArray, args: items}, nil
	case "cast":
		if err := p.expect("("); err != nil {
			return nil, err
		}
		operand, err := p.or()
		if err != nil {
			return nil, err
		}
		if !p.word("as") {
			return nil, fmt.Errorf("a CAST without AS")
		}
		name, err := p.typeName()
		if err != nil {
			return nil, err
		}
		return &node{kind: nodeCast, op: name, args: []*node{operand}}, p.expect(")")
	case "and", "or", "not", "is", "in", "between", "like", "ilike", "any", "all", "some", "case", "when", "then", "else", "end", "exists", "select":
		return nil, fmt.Errorf("unexpected keyword %q", word)
	}
	return p.nameOrCall(word)
}

// nameOrCall reads a column, or a function call when a parenthesis follows.
// The lexer refuses the dot of a qualified name: a CHECK names its own
// table's columns.
func (p *parser) nameOrCall(name string) (*node, error) {
	if !p.punct("(") {
		return &node{kind: nodeColumn, text: name}, nil
	}
	if p.word("distinct") {
		return nil, fmt.Errorf("an aggregate")
	}
	args, err := p.list(")")
	if err != nil {
		return nil, err
	}
	return &node{kind: nodeCall, op: name, args: args}, nil
}

// list reads expressions separated by commas up to the closing punctuation.
func (p *parser) list(closing string) ([]*node, error) {
	var items []*node
	if p.punct(closing) {
		return items, nil
	}
	for {
		item, err := p.or()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if p.punct(closing) {
			return items, nil
		}
		if err := p.expect(","); err != nil {
			return nil, err
		}
	}
}

// multiWordTypes are the type names of more than one word, each word by
// word.
var multiWordTypes = [][]string{
	{"double", "precision"},
	{"character", "varying"},
	{"timestamp", "with", "time", "zone"},
	{"timestamp", "without", "time", "zone"},
}

// typeName reads the type a cast names: one or more words, a modifier in
// parentheses and array brackets, and returns it as written in lower case
// with single spaces.
func (p *parser) typeName() (string, error) {
	t := p.next()
	if t.kind != tokenWord && t.kind != tokenName {
		return "", fmt.Errorf("a cast without a type")
	}
	words := []string{t.text}
	for _, multi := range multiWordTypes {
		if multi[0] != t.text {
			continue
		}
		matched := true
		for i, w := range multi[1:] {
			if !p.wordAhead(i, w) {
				matched = false
				break
			}
		}
		if matched {
			p.pos += len(multi) - 1
			words = multi
			break
		}
	}
	var name strings.Builder
	name.WriteString(strings.Join(words, " "))
	if p.punct("(") {
		var modifiers []string
		for {
			m := p.next()
			if m.kind != tokenNumber {
				return "", fmt.Errorf("a type modifier that is not a number")
			}
			modifiers = append(modifiers, m.text)
			if p.punct(")") {
				break
			}
			if err := p.expect(","); err != nil {
				return "", err
			}
		}
		name.WriteString("(" + strings.Join(modifiers, ",") + ")")
	}
	for p.punct("[") {
		if err := p.expect("]"); err != nil {
			return "", err
		}
		name.WriteString("[]")
	}
	return name.String(), nil
}
