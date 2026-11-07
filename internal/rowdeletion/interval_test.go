package rowdeletion_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The server stores its own spelling of a declared interval, measured on the
// emulator behind PGAdapter; each pair is one interval, so a declaration and
// the server's spelling of it compare equal.
func TestEqualIntervals_TheServersSpellings(t *testing.T) {
	tests := []struct{ declared, stored string }{
		{declared: "1 days", stored: "24 HOURS"},
		{declared: "7 days", stored: "7 DAYS"},
		{declared: "14 days", stored: "2 WEEKS"},
		{declared: "29 days", stored: "4 WEEKS 24 HOURS"},
		{declared: "30 days", stored: "4 WEEKS 2 DAYS"},
		{declared: "31 days", stored: "1 MONTHS 24 HOURS"},
		{declared: "60 days", stored: "2 MONTHS"},
		{declared: "365 days", stored: "12 MONTHS 5 DAYS"},
	}
	for _, test := range tests {
		t.Run(test.declared, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(rowdeletion.EqualIntervals(test.declared, test.stored), qt.IsTrue)
		})
	}
}

func TestEqualIntervals_Differences(t *testing.T) {
	tests := []struct{ name, a, b string }{
		{name: "another number of days", a: "30 days", b: "4 WEEKS 3 DAYS"},
		{name: "two unreadable spellings", a: "P30D", b: "P31D"},
		{name: "a readable and an unreadable spelling", a: "30 days", b: "P30D"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(rowdeletion.EqualIntervals(test.a, test.b), qt.IsFalse)
		})
	}
}

// Two spellings neither side reads are one interval when they are the same
// text, so the comparison converges on them.
func TestEqualIntervals_UnreadableSpellingsCompareAsText(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(rowdeletion.EqualIntervals(" P30D", "p30d "), qt.IsTrue)
}

func TestIntervalHours_HappyPath(t *testing.T) {
	tests := []struct {
		interval string
		hours    int64
	}{
		{interval: "1 month", hours: 720},
		{interval: "2 WEEKS 1 day", hours: 360},
		{interval: "48 hours", hours: 48},
		{interval: "0 days", hours: 0},
	}
	for _, test := range tests {
		t.Run(test.interval, func(t *testing.T) {
			c := testcheck.New(t)
			hours, ok := rowdeletion.IntervalHours(test.interval)
			c.Assert(ok, qt.IsTrue)
			c.Assert(hours, qt.Equals, test.hours)
		})
	}
}

func TestIntervalHours_FailurePath(t *testing.T) {
	for _, interval := range []string{"", "30", "days 30", "30 minutes", "P30D", "1.5 days"} {
		t.Run(interval, func(t *testing.T) {
			c := testcheck.New(t)
			hours, ok := rowdeletion.IntervalHours(interval)
			c.Assert(ok, qt.IsFalse)
			c.Assert(hours, qt.Equals, int64(0))
		})
	}
}

func TestValidateInterval_HappyPath(t *testing.T) {
	for _, interval := range []string{"30 days", "0 days", "4 WEEKS 2 DAYS", "48 hours"} {
		t.Run(interval, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(rowdeletion.ValidateInterval(interval), qt.IsNil)
		})
	}
}

// An interval the server refuses, or one the provider could not compare with
// the server's spelling of it, is refused without quoting it.
func TestValidateInterval_FailurePath(t *testing.T) {
	tests := []struct{ interval, wantErr string }{
		{interval: "1 hour", wantErr: `the interval is not a whole number of days, which Spanner refuses`},
		{interval: "-2 days", wantErr: `the interval is negative`},
		{interval: "P30D", wantErr: `the interval is not a number of months, weeks, days or hours, such as 30 days`},
	}
	for _, test := range tests {
		t.Run(test.interval, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(rowdeletion.ValidateInterval(test.interval), qt.ErrorMatches, test.wantErr)
		})
	}
}

func TestValidateDeclared_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		policy  rowdeletion.Policy
		wantErr string
	}{
		{name: "no column", policy: rowdeletion.Policy{Interval: "30 days"}, wantErr: `the row deletion policy of table "t": the policy names no column`},
		{name: "no interval", policy: rowdeletion.Policy{Column: "c", Interval: " "}, wantErr: `the row deletion policy of table "t": the policy names no interval`},
		{name: "a quote", policy: rowdeletion.Policy{Column: "c", Interval: "30 days'"},
			wantErr: `the row deletion policy of table "t": the interval contains a quote, which the clause cannot carry`},
		{name: "a NUL byte", policy: rowdeletion.Policy{Column: "c\x00", Interval: "30 days"}, wantErr: `the row deletion policy of table "t": the column contains a NUL byte`},
		{name: "part of a day", policy: rowdeletion.Policy{Column: "c", Interval: "36 hours"},
			wantErr: `the row deletion policy of table "t": the interval is not a whole number of days, which Spanner refuses`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(rowdeletion.ValidateDeclared(`"t"`, test.policy), qt.ErrorMatches, test.wantErr)
		})
	}
}
