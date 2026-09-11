package v1

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRecoverParamsDays(t *testing.T) {
	tests := []struct {
		name    string
		params  RecoverParams
		want    []string
		wantErr error
	}{
		{
			name:   "DayTo defaults to DayFrom",
			params: RecoverParams{DayFrom: "2026-04-30"},
			want:   []string{"2026-04-30"},
		},
		{
			name:   "inclusive range",
			params: RecoverParams{DayFrom: "2026-04-29", DayTo: "2026-05-01"},
			want:   []string{"2026-04-29", "2026-04-30", "2026-05-01"},
		},
		{
			name:   "same day both ends",
			params: RecoverParams{DayFrom: "2026-04-30", DayTo: "2026-04-30"},
			want:   []string{"2026-04-30"},
		},
		{
			name:    "inverted range",
			params:  RecoverParams{DayFrom: "2026-05-01", DayTo: "2026-04-30"},
			wantErr: ErrInvalidRecoverRange,
		},
		{
			name:    "wider than the cap",
			params:  RecoverParams{DayFrom: "2026-01-01", DayTo: "2026-06-01"},
			wantErr: ErrInvalidRecoverRange,
		},
		{
			name:    "unparseable DayFrom",
			params:  RecoverParams{DayFrom: "30-04-2026"},
			wantErr: ErrInvalidDay,
		},
		{
			name:    "unparseable DayTo",
			params:  RecoverParams{DayFrom: "2026-04-30", DayTo: "not-a-day"},
			wantErr: ErrInvalidDay,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.params.days()
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("days() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("days() unexpected error: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("days() = %v, want %v", got, tt.want)
			}
		})
	}
}

// A window exactly at the cap is allowed; one day more is not.
func TestRecoverParamsDaysCapBoundary(t *testing.T) {
	atCap := RecoverParams{DayFrom: "2026-01-01", DayTo: "2026-01-31"} // 31 days
	days, err := atCap.days()
	if err != nil {
		t.Fatalf("days() at the cap returned %v", err)
	}
	if len(days) != maxRecoverDays {
		t.Fatalf("days() returned %d days, want %d", len(days), maxRecoverDays)
	}

	overCap := RecoverParams{DayFrom: "2026-01-01", DayTo: "2026-02-01"} // 32 days
	if _, err := overCap.days(); !errors.Is(err, ErrInvalidRecoverRange) {
		t.Fatalf("days() one past the cap error = %v, want %v", err, ErrInvalidRecoverRange)
	}
}

// RecoverTrades validates before it reaches the Core or the database.
func TestRecoverTradesValidation(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	if _, err := c.RecoverTrades(ctx, nil, nil); !errors.Is(err, ErrPartnerIDRequired) {
		t.Errorf("RecoverTrades(nil) error = %v, want %v", err, ErrPartnerIDRequired)
	}
	if _, err := c.RecoverTrades(ctx, &RecoverParams{DayFrom: "2026-04-30"}, nil); !errors.Is(err, ErrPartnerIDRequired) {
		t.Errorf("RecoverTrades() without partner error = %v, want %v", err, ErrPartnerIDRequired)
	}
	if _, err := c.RecoverTrades(ctx, &RecoverParams{PartnerId: "p1", DayFrom: "nope"}, nil); !errors.Is(err, ErrInvalidDay) {
		t.Errorf("RecoverTrades() bad day error = %v, want %v", err, ErrInvalidDay)
	}
}
