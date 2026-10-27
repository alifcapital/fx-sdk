package v1

import (
	"context"
	"errors"
	"testing"
)

// Reconcile validates its parameters before it touches the database or the
// Core, so a zero Client is enough to exercise the guard clauses.
func TestReconcileValidation(t *testing.T) {
	tests := []struct {
		name   string
		params *ReconcileParams
		want   error
	}{
		{"nil params", nil, ErrPartnerIDRequired},
		{"missing partner", &ReconcileParams{Day: "2026-04-30", Hour: 10}, ErrPartnerIDRequired},
		{"hour too low", &ReconcileParams{PartnerId: "p1", Day: "2026-04-30", Hour: -1}, ErrInvalidHour},
		{"hour too high", &ReconcileParams{PartnerId: "p1", Day: "2026-04-30", Hour: 24}, ErrInvalidHour},
		{"empty day", &ReconcileParams{PartnerId: "p1", Hour: 0}, ErrInvalidDay},
		{"day not a date", &ReconcileParams{PartnerId: "p1", Day: "30-04-2026", Hour: 0}, ErrInvalidDay},
	}

	c := &Client{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.Reconcile(context.Background(), tt.params)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Reconcile() error = %v, want %v", err, tt.want)
			}
		})
	}
}

// The local decimal columns are NUMERIC(28,6) and come back zero-padded, while
// the Core sends its own formatting. sameDecimal has to see through that or
// every trade reads as mismatched.
func TestSameDecimal(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"10.85", "10.850000", true},
		{"1000", "1000.000000", true},
		{"0", "0.000000", true},
		{"-1.5", "-1.500000", true},
		{"10.85", "10.86", false},
		{"1000", "1000.000001", false},
		// Unparseable values fall back to an exact comparison, so a malformed
		// value is reported as a difference instead of silently matching.
		{"", "0.000000", false},
		{"n/a", "n/a", true},
		{"n/a", "0", false},
	}
	for _, tt := range tests {
		if got := sameDecimal(tt.a, tt.b); got != tt.want {
			t.Errorf("sameDecimal(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCompareTrade(t *testing.T) {
	core := Trade{TradeId: 1, OrderId: 2, RefId: 3, Side: Buy, FilledQuantity: "100", ExecutionRate: "10.85"}
	local := LocalTrade{TradeId: 1, OrderId: 2, RefId: 3, Side: Buy, FilledQuantity: "100.000000", ExecutionRate: "10.850000"}

	if fields := compareTrade(core, local); len(fields) != 0 {
		t.Fatalf("identical trades reported as differing on %v", fields)
	}

	differing := local
	differing.RefId = 99
	differing.Side = Sell
	differing.FilledQuantity = "50.000000"
	differing.ExecutionRate = "11.000000"
	got := compareTrade(core, differing)
	want := []string{"ref_id", "side", "filled_quantity", "execution_rate"}
	if len(got) != len(want) {
		t.Fatalf("compareTrade() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("compareTrade() = %v, want %v", got, want)
		}
	}
}

func TestTradeDiffDivergent(t *testing.T) {
	// Unsettled trades alone are not a divergence: both sides hold the same
	// trade, the partner has simply not finished settling it.
	unsettledOnly := &TradeDiff{Unsettled: []LocalTrade{{TradeId: 1}}}
	if unsettledOnly.Divergent() {
		t.Error("Divergent() = true for an hour whose only anomaly is an unsettled trade")
	}
	if !(&TradeDiff{MissingLocally: []Trade{{TradeId: 1}}}).Divergent() {
		t.Error("Divergent() = false with a trade missing locally")
	}
	if !(&TradeDiff{MissingRemotely: []LocalTrade{{TradeId: 1}}}).Divergent() {
		t.Error("Divergent() = false with a trade missing remotely")
	}
	if !(&TradeDiff{Mismatched: []TradeMismatch{{TradeId: 1}}}).Divergent() {
		t.Error("Divergent() = false with a mismatched trade")
	}
}

// GetTrades and ReconcileDiff validate before touching the database or the
// Core, so a zero Client exercises the guard clauses.
func TestDrillDownValidation(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	if _, err := c.GetTrades(ctx, nil); !errors.Is(err, ErrPartnerIDRequired) {
		t.Errorf("GetTrades(nil) error = %v, want %v", err, ErrPartnerIDRequired)
	}
	if _, err := c.GetTrades(ctx, &GetTradesParams{PartnerId: "p1", Day: "30-04-2026"}); !errors.Is(err, ErrInvalidDay) {
		t.Errorf("GetTrades() bad day error = %v, want %v", err, ErrInvalidDay)
	}
	if _, err := c.ReconcileDiff(ctx, nil); !errors.Is(err, ErrPartnerIDRequired) {
		t.Errorf("ReconcileDiff(nil) error = %v, want %v", err, ErrPartnerIDRequired)
	}
	if _, err := c.ReconcileDiff(ctx, &ReconcileParams{PartnerId: "p1", Day: "2026-04-30", Hour: 24}); !errors.Is(err, ErrInvalidHour) {
		t.Errorf("ReconcileDiff() bad hour error = %v, want %v", err, ErrInvalidHour)
	}
}

func TestRepairResultResolved(t *testing.T) {
	// An hour that reconciled on the first check never ran a repair, so After
	// stays nil and the hour is still resolved.
	clean := &RepairResult{Before: &ReconcileResult{Matched: true}}
	if !clean.Resolved() {
		t.Error("Resolved() = false for an hour that matched on the first check")
	}
	// Mismatched, then repaired into a match.
	repaired := &RepairResult{
		Before:    &ReconcileResult{Matched: false},
		After:     &ReconcileResult{Matched: true},
		Recovered: 1,
		Settled:   1,
	}
	if !repaired.Resolved() {
		t.Error("Resolved() = false after a repair that matched again")
	}
	// Repaired, but the hour still diverges — needs a ReconcileDiff.
	stillBad := &RepairResult{
		Before: &ReconcileResult{Matched: false},
		After:  &ReconcileResult{Matched: false},
	}
	if stillBad.Resolved() {
		t.Error("Resolved() = true for an hour that still mismatches after a repair")
	}
	// Mismatched and the repair moved nothing, so After was never taken.
	noop := &RepairResult{Before: &ReconcileResult{Matched: false}}
	if noop.Resolved() {
		t.Error("Resolved() = true for a mismatch no repair touched")
	}
}

// ReconcileRepair validates through Reconcile before it touches the database,
// the Core or the handler, so a zero Client exercises the guard clauses.
func TestReconcileRepairValidation(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	if _, err := c.ReconcileRepair(ctx, nil, nil); !errors.Is(err, ErrPartnerIDRequired) {
		t.Errorf("ReconcileRepair(nil) error = %v, want %v", err, ErrPartnerIDRequired)
	}
	if _, err := c.ReconcileRepair(ctx, &ReconcileParams{PartnerId: "p1", Day: "2026-04-30", Hour: 24}, nil); !errors.Is(err, ErrInvalidHour) {
		t.Errorf("ReconcileRepair() bad hour error = %v, want %v", err, ErrInvalidHour)
	}
	if _, err := c.ReconcileRepair(ctx, &ReconcileParams{PartnerId: "p1", Day: "30-04-2026", Hour: 0}, nil); !errors.Is(err, ErrInvalidDay) {
		t.Errorf("ReconcileRepair() bad day error = %v, want %v", err, ErrInvalidDay)
	}
}
