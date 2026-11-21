package health

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Date is a calendar date with no time zone, stored as midnight UTC so that
// day arithmetic never trips over DST.
type Date struct{ t time.Time }

func NewDate(y int, m time.Month, d int) Date { return Date{time.Date(y, m, d, 0, 0, 0, 0, time.UTC)} }

func DateOf(t time.Time) Date { return NewDate(t.Year(), t.Month(), t.Day()) }

func (d Date) String() string       { return d.t.Format("2006-01-02") }
func (d Date) AddDays(n int) Date   { return Date{d.t.AddDate(0, 0, n)} }
func (d Date) Before(o Date) bool   { return d.t.Before(o.t) }
func (d Date) DaysUntil(o Date) int { return int(o.t.Sub(d.t).Hours() / 24) }
func (d Date) In(loc *time.Location) time.Time {
	return time.Date(d.t.Year(), d.t.Month(), d.t.Day(), 0, 0, 0, 0, loc)
}

// civilDateTime is the API's CivilDateTime JSON for midnight on d.
func (d Date) civilDateTime() map[string]any {
	return map[string]any{
		"date": map[string]int{"year": d.t.Year(), "month": int(d.t.Month()), "day": d.t.Day()},
		"time": map[string]int{},
	}
}

var relDays = regexp.MustCompile(`^([+-]\d+)d$`)

// ParseDate accepts YYYY-MM-DD, "today", "yesterday" or a relative offset
// like "-7d", resolved against now in loc.
func ParseDate(s string, now time.Time, loc *time.Location) (Date, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	today := DateOf(now.In(loc))
	switch s {
	case "today", "":
		return today, nil
	case "yesterday":
		return today.AddDays(-1), nil
	}
	if m := relDays.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		return today.AddDays(n), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Date{}, fmt.Errorf("invalid date %q: use YYYY-MM-DD, today, yesterday or -Nd", s)
	}
	return DateOf(t), nil
}

// ParseInstant accepts an RFC 3339 timestamp, "now", or anything ParseDate
// accepts (meaning local midnight at the start of that day).
func ParseInstant(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if strings.EqualFold(s, "now") {
		return now, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", s, loc); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, loc); err == nil {
		return t, nil
	}
	d, err := ParseDate(s, now, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q: use RFC 3339 (2026-11-20T08:00:00+01:00), YYYY-MM-DDTHH:MM (local), a date, or now", s)
	}
	return d.In(loc), nil
}

// durationString renders d as the API's Duration JSON ("3600s").
func durationString(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}
