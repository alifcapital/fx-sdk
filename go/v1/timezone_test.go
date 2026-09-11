package v1

import (
	"testing"
	"time"
)

func TestTimeZoneOffset(t *testing.T) {
	_, offset := time.Now().In(TimeZone).Zone()
	if offset != 5*3600 {
		t.Fatalf("TimeZone offset = %ds, want %ds (UTC+05:00)", offset, 5*3600)
	}
}

// The whole point of pinning the offset is that the day and hour do not come
// from the host clock, so every case below feeds an instant expressed in some
// other zone and expects the +05 answer.
func TestTodayAndPreviousHour(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		wantDay  string
		wantPrev string
		wantHour int
	}{
		{
			// 16:05 in +05. Same calendar day everywhere involved.
			name:     "midday",
			now:      time.Date(2026, 4, 30, 11, 5, 0, 0, time.UTC),
			wantDay:  "2026-04-30",
			wantPrev: "2026-04-30",
			wantHour: 15,
		},
		{
			// 02:30 on May 1 in +05, but still 21:30 on Apr 30 in UTC. A host
			// or session in UTC would report the wrong day here.
			name:     "past midnight in +05, previous day in UTC",
			now:      time.Date(2026, 4, 30, 21, 30, 0, 0, time.UTC),
			wantDay:  "2026-05-01",
			wantPrev: "2026-05-01",
			wantHour: 1,
		},
		{
			// 00:30 in +05: the last completed hour is 23 of the *previous*
			// day, which is the rollover PreviousHour has to get right.
			name:     "just past midnight rolls the day back",
			now:      time.Date(2026, 4, 30, 19, 30, 0, 0, time.UTC),
			wantDay:  "2026-05-01",
			wantPrev: "2026-04-30",
			wantHour: 23,
		},
		{
			// Exactly midnight +05, expressed from a zone west of UTC.
			name:     "midnight boundary from a western zone",
			now:      time.Date(2026, 4, 30, 15, 0, 0, 0, time.FixedZone("-04", -4*3600)),
			wantDay:  "2026-05-01",
			wantPrev: "2026-04-30",
			wantHour: 23,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := today(tt.now); got != tt.wantDay {
				t.Errorf("today() = %q, want %q", got, tt.wantDay)
			}
			day, hour := previousHour(tt.now)
			if day != tt.wantPrev || hour != tt.wantHour {
				t.Errorf("previousHour() = %q h%02d, want %q h%02d",
					day, hour, tt.wantPrev, tt.wantHour)
			}
		})
	}
}

// PreviousHour must always hand Reconcile parameters it accepts.
func TestPreviousHourIsAValidWindow(t *testing.T) {
	day, hour := PreviousHour()
	p := &ReconcileParams{PartnerId: "p1", Day: day, Hour: hour}
	from, to, err := p.window()
	if err != nil {
		t.Fatalf("window() for PreviousHour() = %v", err)
	}
	if from == "" || to == "" || from >= to {
		t.Fatalf("window() = [%q, %q), want an ordered non-empty range", from, to)
	}
}

// The SQL anchor must stay the INTERVAL form: PostgreSQL reads the bare string
// '+05' with the inverted POSIX sign convention.
func TestAtTZUsesIntervalForm(t *testing.T) {
	if atTZ != `AT TIME ZONE INTERVAL '+05:00'` {
		t.Fatalf("atTZ = %q, want the INTERVAL form", atTZ)
	}
}
