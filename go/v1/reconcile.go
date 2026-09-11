package v1

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/quagmt/udecimal"

	"github.com/alifcapital/fx-sdk/go/forexv1"
)

const (
	// reconcileWindow is the length of one reconciliation slot. The
	// reconciliations table is keyed (dt, hour), so a slot is exactly one hour.
	reconcileWindow = time.Hour
	// dayLayout is the YYYY-MM-DD form used for every date on the wire and in
	// the local tables.
	dayLayout = "2006-01-02"
	// timestampLayout is the form the Core expects for the dt_from / dt_to
	// bounds of a reconciliation window.
	timestampLayout = "2006-01-02 15:04:05"
)

// window returns the [Hour:00:00, Hour+1:00:00) bounds of the slot, formatted
// the way the Core expects them. Both bounds are wall clock in the SDK
// timezone (UTC+05:00, see TimeZone) and are sent to the Core in that form, so
// the two sides mean the same hour regardless of how either host or either
// database session is configured.
//
// It also doubles as the parameter check: an hour outside 0..23 or a Day that
// is not a date has no window.
func (p *ReconcileParams) window() (from, to string, err error) {
	if p.Hour < 0 || p.Hour > 23 {
		return "", "", ErrInvalidHour
	}
	day, err := time.ParseInLocation(dayLayout, p.Day, TimeZone)
	if err != nil {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidDay, p.Day)
	}
	start := day.Add(time.Duration(p.Hour) * time.Hour)
	return start.Format(timestampLayout), start.Add(reconcileWindow).Format(timestampLayout), nil
}

// Reconcile compares one hour of locally stored trades against the Core and
// records the outcome in the reconciliations table.
//
// The comparison is an order-independent XOR checksum over the trades in the
// window:
//
//	bit_xor(hashint8(trade_id) # hashint8(order_id))
//
// Every stored trade contributes, settled or not. The question reconciliation
// answers is whether the two sides hold the same set of trades — whether
// client_trades and the Core's settlements agree — and nothing else. Whether
// the partner has since moved the money for a trade is local bookkeeping the
// Core knows nothing about, so folding it into the checksum would report a
// divergence where the data is in fact identical. That flag has its own
// mechanism: settled = FALSE is what RetryUnsettled re-runs.
//
// Reconcile is idempotent: it upserts the (dt, hour) row, so re-running an hour
// is safe. Once a row is marked is_done = TRUE it stays done, even if a later
// run diverges again — that flag is how a partner records "this hour has been
// investigated and closed", and Reconcile never takes it back.
//
// Reconcile a *completed* hour. Running it against the hour currently in
// progress compares a moving local set against a moving remote one and will
// report spurious mismatches. Typical use is a ticker a few minutes past the
// hour, reconciling the hour that just closed:
//
//	prev := time.Now().Add(-time.Hour)
//	res, err := c.Reconcile(ctx, &ReconcileParams{
//		PartnerId: partnerId,
//		Day:       prev.Format("2006-01-02"),
//		Hour:      prev.Hour(),
//	})
//
// A mismatch is reported through ReconcileResult.Matched, not as an error; an
// error means the check itself could not be completed. The XOR checksum tells
// you *that* the hour diverged, never *which* trades diverged.
//
// Reconcile only checks. ReconcileRepair is the same check that then fixes what
// it can — use it if you want a mismatched hour repaired automatically instead
// of only reported.
func (c *Client) Reconcile(ctx context.Context, p *ReconcileParams) (*ReconcileResult, error) {
	if p == nil || p.PartnerId == "" {
		return nil, ErrPartnerIDRequired
	}
	dtFrom, dtTo, err := p.window()
	if err != nil {
		return nil, err
	}

	// 1. Local checksum. bit_xor over an empty window is NULL, which would be
	// indistinguishable from a genuine zero checksum, so it is folded to 0 and
	// the row count is carried separately in info to tell the two apart.
	var (
		localHash   int64
		localTrades int64
	)
	// executed_at is TIMESTAMPTZ while the bounds are a plain date plus hours,
	// so the comparison would otherwise resolve them in the session timezone.
	// atTZ pins them to UTC+05:00, the same hour the Core is asked for below.
	if err := c.db.QueryRow(ctx,
		`SELECT COALESCE(bit_xor(hashint8(trade_id) # hashint8(order_id)), 0)::bigint, COUNT(*)
		   FROM client_trades
		  WHERE trading_day = $1::date
		    AND executed_at >= ($1::date + make_interval(hours => $2::int)) `+atTZ+`
		    AND executed_at <  ($1::date + make_interval(hours => $2::int + 1)) `+atTZ,
		p.Day, p.Hour,
	).Scan(&localHash, &localTrades); err != nil {
		return nil, fmt.Errorf("fx-sdk: local reconciliation hash %s h%02d: %w", p.Day, p.Hour, err)
	}

	// 2. Remote checksum. Transient failures are already retried by the unary
	// retry interceptor, so an error here is terminal for this run — the hour is
	// simply left unreconciled and the next run picks it up again.
	resp, err := c.trade.Reconciliation(ctx, &forexv1.ReconciliationRequest{
		TradingDay: &p.Day,
		DtFrom:     &dtFrom,
		DtTo:       &dtTo,
		PartnerId:  &p.PartnerId,
	})
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: reconciliation %s h%02d: %w", p.Day, p.Hour, err)
	}
	remoteHash := resp.GetHashCheck()
	matched := localHash == remoteHash

	// 3. Record the outcome. is_done means "this hour needs no further
	// attention": a match closes it outright, a mismatch leaves it open for
	// investigation. A row a partner has already closed by hand stays closed.
	info := ReconcileInfo{
		LocalHash:   localHash,
		RemoteHash:  remoteHash,
		LocalTrades: localTrades,
		DtFrom:      dtFrom,
		DtTo:        dtTo,
		CheckedAt:   time.Now().In(TimeZone).Format(time.RFC3339),
	}
	res := &ReconcileResult{
		Day:         p.Day,
		Hour:        p.Hour,
		LocalHash:   localHash,
		RemoteHash:  remoteHash,
		LocalTrades: localTrades,
	}
	if err := c.db.QueryRow(ctx,
		`INSERT INTO reconciliations (dt, hour, is_matched, info, is_done)
		 VALUES ($1::date, $2, $3, $4, $3)
		 ON CONFLICT (hour, dt) DO UPDATE
		    SET is_matched = EXCLUDED.is_matched,
		        info       = EXCLUDED.info,
		        is_done    = reconciliations.is_done OR EXCLUDED.is_done
		 RETURNING is_matched, is_done;`,
		p.Day, int16(p.Hour), matched, info,
	).Scan(&res.Matched, &res.Done); err != nil {
		return nil, fmt.Errorf("fx-sdk: record reconciliation %s h%02d: %w", p.Day, p.Hour, err)
	}
	return res, nil
}

// ReconcileDiff answers the question a mismatched Reconcile leaves open: which
// trades actually diverged. It pulls the Core's trade list for the hour via
// GetTrades, reads the local client_trades rows for the same window, and
// classifies every trade that is not identical on both sides.
//
// The local side is read over the same set the checksum covers — every stored
// trade, settled or not — so the diff explains exactly the hour the checksum
// compared. A trade that is stored but not yet settled is not a divergence at
// all: both sides hold it and the data agrees. It is surfaced through
// TradeDiff.Unsettled as information, because an hour being investigated for
// some other reason is a good moment to notice that money has not moved, and
// Divergent() deliberately ignores it.
//
// It reports; it changes nothing. Neither the local rows nor the
// reconciliations row are touched, so the diff can be re-run as often as needed
// while investigating. ReconcileRepair is the counterpart that acts: run this
// on what it could not resolve.
func (c *Client) ReconcileDiff(ctx context.Context, p *ReconcileParams) (*TradeDiff, error) {
	if p == nil || p.PartnerId == "" {
		return nil, ErrPartnerIDRequired
	}
	dtFrom, dtTo, err := p.window()
	if err != nil {
		return nil, err
	}

	// Reconciliation scope on both sides, so the diff explains exactly the set
	// the checksum compared rather than a partner-sized slice of it.
	raw, err := c.getTradesRaw(ctx, p.PartnerId, p.Day, dtFrom, dtTo, true)
	if err != nil {
		return nil, err
	}
	core := toTrades(raw)

	local, err := c.localTrades(ctx, p)
	if err != nil {
		return nil, err
	}

	diff := &TradeDiff{Day: p.Day, Hour: p.Hour, CoreTrades: len(core), LocalTrades: len(local)}
	seen := make(map[tradeKey]bool, len(core))
	for _, ct := range core {
		key := tradeKey{ct.TradeId, ct.OrderId}
		seen[key] = true
		lt, ok := local[key]
		if !ok {
			diff.MissingLocally = append(diff.MissingLocally, ct)
			continue
		}
		if fields := compareTrade(ct, lt); len(fields) > 0 {
			diff.Mismatched = append(diff.Mismatched, TradeMismatch{
				TradeId: ct.TradeId,
				OrderId: ct.OrderId,
				Fields:  fields,
				Core:    ct,
				Local:   lt,
			})
		}
		if !lt.Settled {
			diff.Unsettled = append(diff.Unsettled, lt)
		}
	}
	for key, lt := range local {
		if !seen[key] {
			diff.MissingRemotely = append(diff.MissingRemotely, lt)
		}
	}
	return diff, nil
}

// ReconcileRepair reconciles one hour and, when it diverges, repairs it: it
// pulls the Core's trades for the same [dt_from, dt_to) window Reconcile just
// compared, stores everything the local database is missing, runs the
// settlement handler for anything not settled yet, and re-checks the hour so
// the reconciliations row records the state *after* the repair rather than the
// state that triggered it.
//
// It is the automatic counterpart to the ReconcileDiff drill-down. Instead of
// telling a human which trades diverged, it fixes the one divergence that is
// mechanically fixable: a trade the Core has that never reached the local
// database (TradeDiff.MissingLocally). That is the case that actually loses
// money, since no settlement ever ran for it.
//
// Settling is a by-product, not the goal. Because every trade of the window
// goes back through the same entry points, a trade that was already stored but
// whose settlement failed gets its handler re-run while the repair walks past
// it. That is free, and idempotent, but it is not what triggers a repair —
// an unsettled trade does not affect the checksum, so an hour whose only
// anomaly is unsettled trades never reaches this function. RetryUnsettled is
// what guarantees those are retried.
//
// Every trade goes through persistTrade and settle, the exact entry points the
// live stream and RecoverTrades use, so a repaired trade is stored, decrements
// its parent order exactly once and settles under the same idempotency
// guarantees. Trades already present are recognised by the ON CONFLICT DO
// NOTHING insert, so re-running a repair is safe and so is running one while
// the stream is live. The handler MUST be idempotent, for the same reason it
// must be on the stream: a trade may be presented to it more than once.
//
// What it cannot fix it leaves alone. A local trade the Core does not have
// (TradeDiff.MissingRemotely) and a trade both sides hold with differing values
// (TradeDiff.Mismatched) are genuine disagreements that need a human; if the
// hour still mismatches, RepairResult.After reports it and ReconcileDiff is the
// next step.
//
// Like Reconcile, run it against a *completed* hour:
//
//	prev := time.Now().Add(-time.Hour)
//	res, err := c.ReconcileRepair(ctx, &ReconcileParams{
//		PartnerId: partnerId,
//		Day:       prev.Format("2006-01-02"),
//		Hour:      prev.Hour(),
//	}, handleTrade)
//	if err == nil && !res.Resolved() {
//		diff, _ := c.ReconcileDiff(ctx, params) // needs a human
//	}
//
// A mismatch is not an error, and neither is a repair that failed to resolve
// it — both are reported through RepairResult. An error means the check or the
// repair could not be completed; the returned RepairResult still reports
// everything done up to that point.
func (c *Client) ReconcileRepair(ctx context.Context, p *ReconcileParams, handler TradeEventHandler) (*RepairResult, error) {
	before, err := c.Reconcile(ctx, p)
	if err != nil {
		return nil, err
	}
	res := &RepairResult{Day: p.Day, Hour: p.Hour, Before: before}
	if before.Matched {
		return res, nil
	}

	// Exactly the bounds Reconcile compared, so the repair covers the window
	// that diverged and nothing else. Reconcile already validated them.
	dtFrom, dtTo, err := p.window()
	if err != nil {
		return res, err
	}
	raw, err := c.getTradesRaw(ctx, p.PartnerId, p.Day, dtFrom, dtTo, true)
	if err != nil {
		return res, err
	}
	res.CoreTrades = len(raw)

	for _, t := range raw {
		event, inserted, settled, perr := c.persistTrade(ctx, t)
		if perr != nil {
			log.Printf("fx-sdk: repair trade %s h%02d trade_id=%d order_id=%d: %v\n",
				p.Day, p.Hour, t.GetId(), t.GetOrderId(), perr)
			res.Failed++
			continue
		}
		if inserted {
			res.Recovered++
		}
		// Settle whatever is not settled yet, whether this call stored it or an
		// earlier delivery did and its settlement failed. This is the branch
		// that clears an Unsettled trade and lets the hour reconcile.
		if settled {
			continue
		}
		if serr := c.settle(ctx, handler, event); serr != nil {
			res.SettleFailed++
			continue
		}
		res.Settled++
	}

	// Nothing moved, so the local checksum cannot have changed. Skip the second
	// round trip and leave After nil — Before is still the current state.
	if res.Recovered == 0 && res.Settled == 0 {
		return res, nil
	}
	after, err := c.Reconcile(ctx, p)
	if err != nil {
		return res, err
	}
	res.After = after
	return res, nil
}

// tradeKey identifies a trade within one trading day, matching the
// client_trades primary key minus the day (the window never spans two days).
type tradeKey struct {
	tradeId int64
	orderId int64
}

// localTrades loads every client_trades row in the window, settled or not,
// keyed for lookup against the Core's list.
//
// There is no partner_id filter, for the same reason the checksum has none:
// the Core's half of a reconciliation covers everything the SDK key traded, so
// the local half must too. Filtering here would drop rows the Core returned
// and report them as MissingLocally on a key that serves several partners.
func (c *Client) localTrades(ctx context.Context, p *ReconcileParams) (map[tradeKey]LocalTrade, error) {
	// Same UTC+05:00 anchoring as the checksum, and executed_at is rendered in
	// that zone too so LocalTrade.ExecutedAt lines up with the window bounds a
	// caller sees rather than with whatever the session timezone happens to be.
	rows, err := c.db.Query(ctx,
		`SELECT trade_id, order_id, ref_id, side, filled_quantity::text, execution_rate::text,
		        to_char(executed_at `+atTZ+`, 'YYYY-MM-DD HH24:MI:SS'), ack, settled, settle_attempts,
		        COALESCE(settle_error, '')
		   FROM client_trades
		  WHERE trading_day = $1::date
		    AND executed_at >= ($1::date + make_interval(hours => $2::int)) `+atTZ+`
		    AND executed_at <  ($1::date + make_interval(hours => $2::int + 1)) `+atTZ,
		p.Day, p.Hour)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: query local trades %s h%02d: %w", p.Day, p.Hour, err)
	}
	defer rows.Close()

	local := make(map[tradeKey]LocalTrade)
	for rows.Next() {
		var lt LocalTrade
		if err := rows.Scan(&lt.TradeId, &lt.OrderId, &lt.RefId, &lt.Side, &lt.FilledQuantity,
			&lt.ExecutionRate, &lt.ExecutedAt, &lt.Ack, &lt.Settled, &lt.SettleAttempts,
			&lt.SettleError); err != nil {
			return nil, fmt.Errorf("fx-sdk: scan local trade: %w", err)
		}
		local[tradeKey{lt.TradeId, lt.OrderId}] = lt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fx-sdk: iterate local trades: %w", err)
	}
	return local, nil
}

// compareTrade returns the names of the fields on which the Core's copy of a
// trade and the local copy disagree. Only the fields the partner stores are
// compared; executed_at is left out because the two sides may render the same
// instant differently.
func compareTrade(core Trade, local LocalTrade) []string {
	var fields []string
	if core.RefId != local.RefId {
		fields = append(fields, "ref_id")
	}
	if core.Side != local.Side {
		fields = append(fields, "side")
	}
	if !sameDecimal(core.FilledQuantity, local.FilledQuantity) {
		fields = append(fields, "filled_quantity")
	}
	if !sameDecimal(core.ExecutionRate, local.ExecutionRate) {
		fields = append(fields, "execution_rate")
	}
	return fields
}

// sameDecimal reports whether two decimal strings denote the same value. The
// local columns are NUMERIC(28,6), so they come back zero-padded ("10.850000")
// while the Core sends its own formatting ("10.85") — a byte comparison would
// flag every trade as mismatched. Values that do not parse fall back to an
// exact string comparison so a malformed value is still reported as a
// difference rather than silently treated as equal.
func sameDecimal(a, b string) bool {
	da, err := udecimal.Parse(a)
	if err != nil {
		return a == b
	}
	db, err := udecimal.Parse(b)
	if err != nil {
		return a == b
	}
	return da.Equal(db)
}
