package rowdata

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// The bounds of a numeric value on Spanner's PostgreSQL interface: 131072
// digits before the point and 16383 after it.
const (
	maxIntegerDigits  = 131072
	maxFractionDigits = 16383
)

// decimalParts is a decimal number in its shortest form: the unsigned digits
// without leading zeros, `0` for zero, and the count of them after the point,
// with no trailing zero in the fraction.
type decimalParts struct {
	negative bool
	digits   string
	scale    int64
}

// errNotNumber refuses text that spells no decimal number.
var errNotNumber = errors.New("a text value is not a decimal number; Ptah writes no numeric NaN or infinity")

// parseDecimal reads a decimal number as SQL and JSON write one: an optional
// sign, digits with an optional point, and an optional exponent.
func parseDecimal(text string) (decimalParts, error) {
	negative := strings.HasPrefix(text, "-")
	rest := strings.TrimLeft(text, "+-")
	if len(text)-len(rest) > 1 {
		return decimalParts{}, errNotNumber
	}
	mantissa, exponentText, hasExponent := strings.Cut(strings.ToLower(rest), "e")
	var exponent int64
	if hasExponent {
		parsed, err := strconv.ParseInt(exponentText, 10, 32)
		if err != nil {
			return decimalParts{}, errNotNumber
		}
		exponent = parsed
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	if whole+fraction == "" || !allDigits(whole) || !allDigits(fraction) {
		return decimalParts{}, errNotNumber
	}
	return normalizeDecimal(negative, whole+fraction, int64(len(fraction))-exponent)
}

// normalizedDecimal is a decimal value of the protocol, digits with an
// optional leading minus and the scale that places the point, in its shortest
// form.
func normalizedDecimal(digits string, scale int64) (decimalParts, error) {
	negative := strings.HasPrefix(digits, "-")
	unsigned := strings.TrimPrefix(digits, "-")
	if unsigned == "" || !allDigits(unsigned) {
		return decimalParts{}, errNotNumber
	}
	return normalizeDecimal(negative, unsigned, scale)
}

// normalizeDecimal drops the leading zeros, the trailing zeros of the fraction
// and the sign of zero, and refuses a number beyond the bounds Spanner keeps.
func normalizeDecimal(negative bool, unscaled string, scale int64) (decimalParts, error) {
	trimmed := strings.TrimLeft(unscaled, "0")
	if trimmed == "" {
		return decimalParts{digits: "0"}, nil
	}
	if scale < 0 {
		if int64(len(trimmed))-scale > maxIntegerDigits {
			return decimalParts{}, fmt.Errorf("a decimal value has more than the %d digits before the point a numeric holds", maxIntegerDigits)
		}
		trimmed += strings.Repeat("0", int(-scale))
		scale = 0
	}
	for scale > 0 && strings.HasSuffix(trimmed, "0") {
		trimmed = trimmed[:len(trimmed)-1]
		scale--
	}
	if scale > maxFractionDigits {
		return decimalParts{}, fmt.Errorf("a decimal value has more than the %d digits after the point a numeric holds", maxFractionDigits)
	}
	if int64(len(trimmed))-scale > maxIntegerDigits {
		return decimalParts{}, fmt.Errorf("a decimal value has more than the %d digits before the point a numeric holds", maxIntegerDigits)
	}
	return decimalParts{negative: negative, digits: trimmed, scale: scale}, nil
}

// value is the decimal as the protocol carries it, with no precision: a
// numeric column of Spanner's PostgreSQL interface declares none.
func (d decimalParts) value() *protocol.DataValue {
	digits := d.digits
	if d.negative {
		digits = "-" + digits
	}
	return &protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{
		Digits: digits, Scale: uint32(d.scale)}}} // #nosec G115 -- normalizeDecimal bounds the scale by maxFractionDigits
}

// text writes the decimal with its point, `12.5` for digits 125 and scale 1.
func (d decimalParts) text() string {
	digits := d.digits
	if d.negative {
		digits = "-" + digits
	}
	return decimalText(digits, d.scale)
}

// decimalText places the point scale digits from the right of the digits,
// which may start with a minus.
func decimalText(digits string, scale int64) string {
	negative := strings.HasPrefix(digits, "-")
	unsigned := strings.TrimPrefix(digits, "-")
	if scale > 0 {
		if int64(len(unsigned)) <= scale {
			unsigned = strings.Repeat("0", int(scale)-len(unsigned)+1) + unsigned
		}
		unsigned = unsigned[:int64(len(unsigned))-scale] + "." + unsigned[int64(len(unsigned))-scale:]
	}
	if negative {
		return "-" + unsigned
	}
	return unsigned
}

func allDigits(text string) bool {
	for _, character := range text {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

// errNotJSON refuses text that is not one JSON value.
var errNotJSON = errors.New("a text value is not JSON")

// CanonicalJSON writes JSON text in one canonical form: no white space
// between tokens, the members of an object in byte order of their names, the
// last of two members of one name kept, every number in its shortest decimal
// form and every string with only the escapes JSON requires.
//
// Spanner normalizes a jsonb value as it stores it: it sorts the members,
// keeps the last of two members of one name, writes 2e0 as 2, 1e2 as 100,
// 1E-3 as 0.001 and -0.0 as 0.0 but keeps 1.50, and decodes A to A. It
// refuses a \u escape of a character outside ASCII (`unsupported Unicode
// escape sequence`) and stores the character itself, so the canonical form
// never escapes one. Both a declared and a read value are written in this
// form, so a stored value and its declaration compare equal whatever spelling
// either has.
func CanonicalJSON(text string) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		return "", errNotJSON
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errNotJSON
	}
	var out strings.Builder
	if err := writeJSON(&out, parsed); err != nil {
		return "", err
	}
	return out.String(), nil
}

func writeJSON(out *strings.Builder, value any) error {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(v))
	case json.Number:
		decimal, err := parseDecimal(v.String())
		if err != nil {
			return fmt.Errorf("a JSON number is beyond the range of a jsonb number: %w", err)
		}
		out.WriteString(decimal.text())
	case string:
		writeJSONString(out, v)
	case []any:
		out.WriteByte('[')
		for i, element := range v {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeJSON(out, element); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		out.WriteByte('{')
		for i, name := range slices.Sorted(maps.Keys(v)) {
			if i > 0 {
				out.WriteByte(',')
			}
			writeJSONString(out, name)
			out.WriteByte(':')
			if err := writeJSON(out, v[name]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return errNotJSON
	}
	return nil
}

// jsonEscapes are the short escapes JSON defines for control characters.
var jsonEscapes = map[rune]string{'\b': `\b`, '\f': `\f`, '\n': `\n`, '\r': `\r`, '\t': `\t`, '"': `\"`, '\\': `\\`}

func writeJSONString(out *strings.Builder, text string) {
	out.WriteByte('"')
	for _, character := range text {
		switch escape, found := jsonEscapes[character]; {
		case found:
			out.WriteString(escape)
		case character < 0x20:
			fmt.Fprintf(out, `\u%04x`, character)
		default:
			out.WriteRune(character)
		}
	}
	out.WriteByte('"')
}
