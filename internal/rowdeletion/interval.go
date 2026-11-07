package rowdeletion

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// unitHours is the server's own arithmetic, read off the stored spellings it
// writes: a month is 30 days, a week is 7, a day is 24 hours. Measured on the
// emulator behind PGAdapter, a policy declared as `30 days` reads back as
// `4 WEEKS 2 DAYS`, `60 days` as `2 MONTHS` and `365 days` as `12 MONTHS 5
// DAYS`. Nothing smaller than an hour appears, because the server refuses an
// interval that is not a whole number of days (`TTL interval must be a whole
// number of days`).
//
// This is not a PostgreSQL interval. PostgreSQL keeps months, days and
// microseconds apart, and under that reading `60 days` and the `2 MONTHS` the
// server stores for it differ, so a comparison built on it would plan the same
// ALTER TTL on every run.
var unitHours = map[string]int64{
	"month": 30 * 24, "months": 30 * 24,
	"week": 7 * 24, "weeks": 7 * 24,
	"day": 24, "days": 24,
	"hour": 1, "hours": 1,
}

// IntervalHours reduces an interval to hours. It reports false for a spelling
// it does not read, so a caller can refuse it rather than guess a number.
func IntervalHours(interval string) (int64, bool) {
	fields := strings.Fields(strings.TrimSpace(interval))
	if len(fields) == 0 || len(fields)%2 != 0 {
		return 0, false
	}
	var total int64
	for i := 0; i < len(fields); i += 2 {
		quantity, err := strconv.ParseInt(fields[i], 10, 64)
		if err != nil {
			return 0, false
		}
		hours, known := unitHours[strings.ToLower(fields[i+1])]
		if !known {
			return 0, false
		}
		total += quantity * hours
	}
	return total, true
}

// ValidateInterval refuses an interval the server refuses or the provider
// cannot compare: one it cannot read, a negative one, and one that is not a
// whole number of days. Zero days is accepted, as the server accepts it. The
// message does not quote the interval, because a refusal reaches the
// operator as the provider's reason, and a reason quotes no value a source
// wrote.
//
// An interval the provider cannot read is refused although the server might
// take it: the server stores its own spelling, and a declaration compared with
// that as text would plan the same change on every run.
func ValidateInterval(interval string) error {
	hours, ok := IntervalHours(interval)
	switch {
	case !ok:
		return fmt.Errorf("the interval is not a number of months, weeks, days or hours, such as 30 days")
	case hours < 0:
		return fmt.Errorf("the interval is negative")
	case hours%24 != 0:
		return fmt.Errorf("the interval is not a whole number of days, which Spanner refuses")
	}
	return nil
}

// EqualIntervals reports whether two intervals are the same number of hours.
//
// An interval either side cannot read is compared as text, ignoring case and
// the space around it. That direction converges: two identical spellings are
// one policy, and two that differ plan the change that makes them identical.
// Reporting a difference for every unreadable spelling would plan that change
// on every run.
func EqualIntervals(a, b string) bool {
	left, leftOK := IntervalHours(a)
	right, rightOK := IntervalHours(b)
	if !leftOK || !rightOK {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	return left == right
}

// Equivalent reports whether two policies delete the same rows: the same
// column, compared exactly as the engine compares names, and the same number
// of hours.
func Equivalent(a, b Policy) bool {
	return a.Column == b.Column && EqualIntervals(a.Interval, b.Interval)
}

// ValidateDeclared refuses a policy a declaration cannot ask for: a missing
// column, and an interval the server refuses or the provider cannot compare.
// Its message names the table the policy belongs to through table.
func ValidateDeclared(table string, policy Policy) error {
	if err := validateText(policy); err != nil {
		return fmt.Errorf("the row deletion policy of table %s: %w", table, err)
	}
	if err := ValidateInterval(policy.Interval); err != nil {
		return fmt.Errorf("the row deletion policy of table %s: %w", table, err)
	}
	return nil
}

// validateText refuses a column or an interval the clause cannot carry: an
// empty one, one that is not UTF-8 or holds a NUL byte, and an interval with a
// quote, which would end the literal it is written in.
func validateText(policy Policy) error {
	for _, field := range []struct{ name, value string }{{"column", policy.Column}, {"interval", policy.Interval}} {
		switch {
		case strings.TrimSpace(field.value) == "":
			return fmt.Errorf("the policy names no %s", field.name)
		case !utf8.ValidString(field.value):
			return fmt.Errorf("the %s is not valid UTF-8", field.name)
		case strings.ContainsRune(field.value, 0):
			return fmt.Errorf("the %s contains a NUL byte", field.name)
		}
	}
	if strings.ContainsRune(policy.Interval, '\'') {
		return fmt.Errorf("the interval contains a quote, which the clause cannot carry")
	}
	return nil
}
