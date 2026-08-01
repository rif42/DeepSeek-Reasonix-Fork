package routines

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, input string, now time.Time) Schedule {
	t.Helper()
	s, err := ParseSchedule(input, now)
	if err != nil {
		t.Fatalf("ParseSchedule(%q): %v", input, err)
	}
	return s
}

func TestParseScheduleOneShot(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := mustParse(t, "30m", now)
	if s.Kind != ScheduleOnce || s.RunAt == nil {
		t.Fatalf("expected one-shot, got %+v", s)
	}
	if want := now.Add(30 * time.Minute); !s.RunAt.Equal(want) {
		t.Fatalf("run_at = %v, want %v", s.RunAt, want)
	}
	s = mustParse(t, "2h", now)
	if want := now.Add(2 * time.Hour); !s.RunAt.Equal(want) {
		t.Fatalf("2h run_at = %v, want %v", s.RunAt, want)
	}
	s = mustParse(t, "1d", now)
	if want := now.Add(24 * time.Hour); !s.RunAt.Equal(want) {
		t.Fatalf("1d run_at = %v, want %v", s.RunAt, want)
	}
}

func TestParseScheduleInterval(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := mustParse(t, "every 30m", now)
	if s.Kind != ScheduleInterval || s.Minutes != 30 {
		t.Fatalf("expected interval 30m, got %+v", s)
	}
	s = mustParse(t, "every 2h", now)
	if s.Minutes != 120 {
		t.Fatalf("expected interval 120m, got %+v", s)
	}
}

func TestParseScheduleISOTimestamp(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := mustParse(t, "2026-03-02T08:00:00Z", now)
	if s.Kind != ScheduleOnce || s.RunAt == nil {
		t.Fatalf("expected one-shot from ISO, got %+v", s)
	}
	if want := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC); !s.RunAt.Equal(want) {
		t.Fatalf("run_at = %v, want %v", s.RunAt, want)
	}
}

func TestParseScheduleCron(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := mustParse(t, "0 2 * * *", now)
	if s.Kind != ScheduleCron || s.cron == nil {
		t.Fatalf("expected cron, got %+v", s)
	}
}

func TestParseScheduleInvalid(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{"", "banana", "every", "0 2 * *", "61 * * * *", "0 2 * * * *", "-5m"} {
		if _, err := ParseSchedule(in, now); err == nil {
			t.Errorf("ParseSchedule(%q): expected error, got nil", in)
		}
	}
}

func TestNextRunOnce(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := mustParse(t, "2026-03-02T08:00:00Z", now)
	next := s.NextRun(now)
	if next == nil || !next.Equal(time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("NextRun = %v, want 2026-03-02T08:00:00Z", next)
	}
	// A past one-shot has no next run.
	s = mustParse(t, "2026-01-01T08:00:00Z", now)
	if next := s.NextRun(now); next != nil {
		t.Fatalf("past one-shot NextRun = %v, want nil", next)
	}
	// Once fired, it never fires again.
	s = mustParse(t, "2026-03-02T08:00:00Z", now)
	after := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if next := s.NextRun(after); next != nil {
		t.Fatalf("already-fired one-shot NextRun = %v, want nil", next)
	}
}

func TestNextRunInterval(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := mustParse(t, "every 30m", now)
	next := s.NextRun(now)
	if want := now.Add(30 * time.Minute); next == nil || !next.Equal(want) {
		t.Fatalf("NextRun = %v, want %v", next, want)
	}
	// Subsequent runs advance from the previous fire time.
	next2 := s.NextRun(*next)
	if want := next.Add(30 * time.Minute); next2 == nil || !next2.Equal(want) {
		t.Fatalf("second NextRun = %v, want %v", next2, want)
	}
}

func TestNextRunCron(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 34, 0, 0, time.UTC) // Sunday
	cases := []struct {
		expr  string
		after time.Time
		want  time.Time
	}{
		{"0 2 * * *", now, time.Date(2026, 3, 2, 2, 0, 0, 0, time.UTC)},     // next 02:00
		{"*/5 * * * *", now, time.Date(2026, 3, 1, 12, 35, 0, 0, time.UTC)}, // next 5-min mark
		{"30 8 1 * *", now, time.Date(2026, 4, 1, 8, 30, 0, 0, time.UTC)},   // next 1st of month
		{"0 9 * * 1", now, time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)},     // next Monday (2026-03-02 is Monday)
		{"0 9 * * 0", now, time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)},     // next Sunday (dow=0)
		{"0 9 * * 7", now, time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC)},     // dow=7 == Sunday
	}
	for _, tc := range cases {
		s := mustParse(t, tc.expr, tc.after)
		got := s.NextRun(tc.after)
		if got == nil || !got.Equal(tc.want) {
			t.Errorf("%q NextRun(%v) = %v, want %v", tc.expr, tc.after, got, tc.want)
		}
	}
}

func TestNextRunCronDomOrDowSemantics(t *testing.T) {
	// dom=13 and dow=3 (Wednesday) are both restricted, so a date matches if
	// either matches. From 2026-03-12 (Thursday): the next Wednesday is
	// 2026-03-18, the next 13th is 2026-03-13 — the dom match fires first.
	now := time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC)
	s := mustParse(t, "0 0 13 * 3", now)
	got := s.NextRun(now)
	if want := time.Date(2026, 3, 13, 0, 0, 0, 0, time.UTC); got == nil || !got.Equal(want) {
		t.Fatalf("NextRun = %v, want %v (dom match should fire first)", got, want)
	}
	// And from a date where the dow match comes first: 2026-03-01 (Sunday)
	// with Monday (dow=1) restricted — Monday 2026-03-02 fires before the 13th.
	now2 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s2 := mustParse(t, "0 0 13 * 1", now2)
	got2 := s2.NextRun(now2)
	if want := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC); got2 == nil || !got2.Equal(want) {
		t.Fatalf("NextRun = %v, want %v (dow match should fire first)", got2, want)
	}
}

func TestCronNeverMatchesReturnsNil(t *testing.T) {
	// Feb 31 does not exist; the search horizon passes and nil means "never".
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := mustParse(t, "0 0 31 2 *", now)
	if got := s.NextRun(now); got != nil {
		t.Fatalf("Feb-31 cron NextRun = %v, want nil", got)
	}
}
