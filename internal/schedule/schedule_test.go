package schedule

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseTimes(t *testing.T) {
	got, err := ParseTimes(" 14:30, 02:00 ,02:00,23:59")
	if err != nil {
		t.Fatal(err)
	}
	want := []HHMM{{2, 0}, {14, 30}, {23, 59}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestParseTimesErrors(t *testing.T) {
	for _, s := range []string{"", " ", "2:00", "24:00", "12:60", "ab:cd", "02:00,,03:00", "02:00:00", "-1:00"} {
		if _, err := ParseTimes(s); err == nil {
			t.Errorf("%q: expected error", s)
		}
	}
	_, err := ParseTimes("02:00,25:00")
	if err == nil || !strings.Contains(err.Error(), "25:00") {
		t.Fatalf("error should mention bad item: %v", err)
	}
}

func TestNext(t *testing.T) {
	times := []HHMM{{2, 0}, {14, 0}}
	d := func(day, h, m int) time.Time { return time.Date(2025, 3, day, h, m, 0, 0, time.UTC) }
	tests := []struct {
		name      string
		now, want time.Time
	}{
		{"before first", d(10, 1, 0), d(10, 2, 0)},
		{"between", d(10, 3, 0), d(10, 14, 0)},
		{"wrap to next day", d(10, 15, 0), d(11, 2, 0)},
		{"exact equal goes next", d(10, 2, 0), d(10, 14, 0)},
		{"exact last goes tomorrow", d(10, 14, 0), d(11, 2, 0)},
		{"seconds past", time.Date(2025, 3, 10, 2, 0, 1, 0, time.UTC), d(10, 14, 0)},
		{"sunday (2025-03-09)", d(9, 3, 0), d(9, 14, 0)},
		{"saturday night to sunday", d(8, 15, 0), d(9, 2, 0)},
		{"sunday night to monday", d(9, 15, 0), d(10, 2, 0)},
	}
	if d(9, 0, 0).Weekday() != time.Sunday {
		t.Fatal("test date is not a Sunday")
	}
	for _, tc := range tests {
		if got := Next(tc.now, times); !got.Equal(tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestNextDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Kyiv")
	if err != nil {
		t.Skip("tz data missing")
	}
	// 2025-03-30: clocks jump 03:00 -> 04:00 in Kyiv.
	now := time.Date(2025, 3, 29, 12, 0, 0, 0, loc)
	got := Next(now, []HHMM{{2, 0}})
	if got.Location() != loc || got.Day() != 30 || got.Hour() != 2 {
		t.Fatalf("got %v", got)
	}
	// Next after that is 31st, 02:00 local, 23h after.
	got2 := Next(got, []HHMM{{2, 0}})
	if got2.Day() != 31 || got2.Hour() != 2 || got2.Sub(got) != 24*time.Hour-time.Hour {
		t.Fatalf("got2 %v", got2)
	}
	// Fall back 2025-10-26: result must be strictly after now.
	now = time.Date(2025, 10, 25, 23, 0, 0, 0, loc)
	for i := 0; i < 5; i++ {
		n := Next(now, []HHMM{{3, 30}})
		if !n.After(now) {
			t.Fatalf("not after: %v <= %v", n, now)
		}
		now = n
	}
}
