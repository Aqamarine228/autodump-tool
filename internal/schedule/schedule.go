// Package schedule parses daily run times and computes the next one.
package schedule

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HHMM is a time of day.
type HHMM struct{ H, M int }

var hhmmRe = regexp.MustCompile(`^\d{2}:\d{2}$`)

// ParseTimes parses a comma-separated list of HH:mm values into a sorted,
// de-duplicated slice. At least one value is required.
func ParseTimes(s string) ([]HHMM, error) {
	seen := map[HHMM]bool{}
	var out []HHMM
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if !hhmmRe.MatchString(item) {
			return nil, fmt.Errorf("invalid time %q: want HH:mm", item)
		}
		h, _ := strconv.Atoi(item[:2])
		m, _ := strconv.Atoi(item[3:])
		if h > 23 || m > 59 {
			return nil, fmt.Errorf("invalid time %q: out of range", item)
		}
		t := HHMM{h, m}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].H*60+out[i].M < out[j].H*60+out[j].M
	})
	return out, nil
}

// Next returns the earliest scheduled instant strictly after now, in now's
// location. Every day of the week is scheduled.
func Next(now time.Time, times []HHMM) time.Time {
	var best time.Time
	for d := 0; d <= 2; d++ {
		for _, t := range times {
			c := time.Date(now.Year(), now.Month(), now.Day()+d, t.H, t.M, 0, 0, now.Location())
			if c.After(now) && (best.IsZero() || c.Before(best)) {
				best = c
			}
		}
	}
	return best
}
