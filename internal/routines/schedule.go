package routines

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// UnmarshalJSON restores the runtime-only parsed cron expression when a
// Schedule is loaded from disk. The cron field is json:"-" (not persisted),
// so without this a cron job loaded from the store would have a nil parser
// and its next run would never advance past the first fire — it would run
// exactly once and then stop.
func (s *Schedule) UnmarshalJSON(b []byte) error {
	type plain Schedule
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = Schedule(p)
	if s.Kind == ScheduleCron && s.Expr != "" {
		c, err := parseCron(s.Expr)
		if err != nil {
			return fmt.Errorf("schedule %q: %w", s.Expr, err)
		}
		s.cron = c
	}
	return nil
}

// ParseSchedule parses a schedule string into a Schedule. Accepted forms:
//
//   - "30m", "2h", "1d"         -> one-shot at now + duration
//   - "every 30m", "every 2h"   -> interval (repeating)
//   - "0 2 * * *"               -> 5-field cron expression
//   - RFC3339 timestamp         -> one-shot at that exact time
func ParseSchedule(input string, now time.Time) (Schedule, error) {
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Schedule{}, errors.New("schedule is empty")
	}
	// "every 30m" / "every 2h" -> interval.
	if rest, ok := strings.CutPrefix(strings.ToLower(raw), "every "); ok {
		d, err := parseDurationWord(rest)
		if err != nil {
			return Schedule{}, fmt.Errorf("schedule %q: %w", input, err)
		}
		if d < time.Minute {
			return Schedule{}, fmt.Errorf("schedule %q: interval must be at least 1 minute", input)
		}
		minutes := int(d / time.Minute)
		return Schedule{Kind: ScheduleInterval, Minutes: minutes, Display: raw}, nil
	}
	// ISO / RFC3339 timestamp -> one-shot.
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return Schedule{Kind: ScheduleOnce, RunAt: &t, Display: raw}, nil
	}
	// Plain duration word -> one-shot at now + duration.
	if d, err := parseDurationWord(raw); err == nil {
		t := now.Add(d)
		return Schedule{Kind: ScheduleOnce, RunAt: &t, Display: raw}, nil
	}
	// 5-field cron expression.
	if strings.Count(raw, " ") == 4 {
		spec, err := parseCron(raw)
		if err != nil {
			return Schedule{}, err
		}
		return Schedule{Kind: ScheduleCron, Expr: raw, Display: raw, cron: spec}, nil
	}
	return Schedule{}, fmt.Errorf("schedule %q: unsupported form (use a duration like \"30m\", \"every 30m\", a cron expression like \"0 2 * * *\", or an RFC3339 timestamp)", input)
}

// parseDurationWord parses a bare duration word like "30m", "2h", "1d", "90s".
// Only s/m/h/d units are accepted.
func parseDurationWord(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	num := s[:len(s)-1]
	unit := s[len(s)-1]
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	switch unit {
	case 's':
		return time.Duration(n) * time.Second, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid duration %q (use s/m/h/d units)", s)
	}
}

// NextRun computes the next fire time strictly after `after`. It returns nil
// when the schedule has no further fires (a past one-shot).
func (s Schedule) NextRun(after time.Time) *time.Time {
	switch s.Kind {
	case ScheduleOnce:
		if s.RunAt == nil {
			return nil
		}
		t := s.RunAt.UTC()
		if !t.After(after) {
			return nil
		}
		return &t
	case ScheduleInterval:
		if s.Minutes <= 0 {
			return nil
		}
		t := after.Add(time.Duration(s.Minutes) * time.Minute)
		return &t
	case ScheduleCron:
		if s.cron == nil {
			return nil
		}
		return s.cron.next(after)
	default:
		return nil
	}
}

// cron is a parsed 5-field cron expression. Fields: minute hour dom month dow.
type cron struct {
	minute, hour, dom, month, dow *cronField
}

// cronField is the set of allowed values for one cron field.
type cronField struct {
	allowed []bool // indexed by value; index 0 is the field's minimum
	min     int
}

func (f *cronField) match(v int) bool {
	return v >= f.min && v-f.min < len(f.allowed) && f.allowed[v-f.min]
}

// parseCron parses a 5-field cron expression.
func parseCron(expr string) (*cron, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron %q: expected 5 fields (minute hour dom month dow)", expr)
	}
	minute, err := parseCronField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("cron %q: minute: %w", expr, err)
	}
	hour, err := parseCronField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("cron %q: hour: %w", expr, err)
	}
	dom, err := parseCronField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("cron %q: day-of-month: %w", expr, err)
	}
	month, err := parseCronField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("cron %q: month: %w", expr, err)
	}
	dow, err := parseCronField(fields[4], 0, 7) // 7 == Sunday
	if err != nil {
		return nil, fmt.Errorf("cron %q: day-of-week: %w", expr, err)
	}
	if dow.match(7) {
		// Normalize Sunday=7 to Sunday=0 so matching below is a single check.
		dow.allowed[0] = dow.allowed[0] || dow.allowed[7]
		dow.allowed = dow.allowed[:7]
	}
	return &cron{minute: minute, hour: hour, dom: dom, month: month, dow: dow}, nil
}

// parseCronField expands one cron field expression (lists, ranges, steps,
// wildcards) into the set of allowed values in [min, max].
func parseCronField(expr string, min, max int) (*cronField, error) {
	f := &cronField{allowed: make([]bool, max-min+1), min: min}
	seen := map[int]bool{}
	add := func(v int) error {
		if v < min || v > max {
			return fmt.Errorf("value %d out of range [%d,%d]", v, min, max)
		}
		if !seen[v] {
			seen[v] = true
			f.allowed[v-min] = true
		}
		return nil
	}
	for part := range strings.SplitSeq(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty list element in %q", expr)
		}
		step := 1
		base := part
		if field, rest, ok := strings.Cut(part, "/"); ok {
			var err error
			step, err = strconv.Atoi(rest)
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("invalid step in %q", part)
			}
			base = field
		}
		switch {
		case base == "*":
			for v := min; v <= max; v += step {
				if err := add(v); err != nil {
					return nil, err
				}
			}
		case strings.Contains(base, "-"):
			ends := strings.SplitN(base, "-", 2)
			lo, err1 := strconv.Atoi(ends[0])
			hi, err2 := strconv.Atoi(ends[1])
			if err1 != nil || err2 != nil || lo > hi {
				return nil, fmt.Errorf("invalid range %q", base)
			}
			for v := lo; v <= hi; v += step {
				if err := add(v); err != nil {
					return nil, err
				}
			}
		default:
			v, err := strconv.Atoi(base)
			if err != nil {
				return nil, fmt.Errorf("invalid value %q", base)
			}
			if err := add(v); err != nil {
				return nil, err
			}
		}
	}
	return f, nil
}

// next returns the next fire time strictly after `after`, searching at most
// cronSearchDays ahead. Standard cron semantics apply: when both day-of-month
// and day-of-week are restricted, a date matches if either matches.
const cronSearchDays = 400

func (c *cron) next(after time.Time) *time.Time {
	after = after.UTC().Truncate(time.Minute)
	start := after.Add(time.Minute) // strictly after
	for day := 0; day <= cronSearchDays; day++ {
		t := start.AddDate(0, 0, day)
		if !c.dayMatches(t) {
			continue
		}
		// Candidate first minute of the day; iterate minutes 0..1439.
		dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
		for m := range 1440 {
			cand := dayStart.Add(time.Duration(m) * time.Minute)
			if !cand.After(after) {
				continue
			}
			if c.minute.match(cand.Minute()) && c.hour.match(cand.Hour()) {
				return &cand
			}
		}
	}
	return nil
}

// dayMatches reports whether the date matches month + day-of-month +
// day-of-week constraints.
func (c *cron) dayMatches(t time.Time) bool {
	if !c.month.match(int(t.Month())) {
		return false
	}
	domRestricted := !isAll(c.dom)
	dowRestricted := !isAll(c.dow)
	domMatch := c.dom.match(t.Day())
	dowMatch := c.dow.match(int(t.Weekday()))
	if domRestricted && dowRestricted {
		return domMatch || dowMatch
	}
	if domRestricted {
		return domMatch
	}
	if dowRestricted {
		return dowMatch
	}
	return true
}

func isAll(f *cronField) bool {
	for _, v := range f.allowed {
		if !v {
			return false
		}
	}
	return true
}
