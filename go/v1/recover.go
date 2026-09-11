package v1

import (
	"context"
	"fmt"
	"log"
	"time"
)

// maxRecoverDays caps how wide a window RecoverTrades will pull in one call.
// Recovery is a catch-up after downtime, not a backfill of history: each day is
// one round trip to the Core and re-walks every trade of that day. A partner
// that genuinely needs more can call RecoverTrades once per chunk.
const maxRecoverDays = 31

// RecoverTrades pulls the trades the Core recorded over a window and processes
// every one the local database is missing. It is the catch-up path for a
// partner that was down, disconnected or restarted after a while: the trade
// stream can only deliver what the Core still has queued, so anything older is
// fetched rather than waited for.
//
// Each trade goes through persistTrade and settle — the exact entry points the
// live stream uses — so a recovered trade is stored, decrements its parent
// order and settles under the same rules and the same idempotency guarantees.
// Trades already present are recognised by the ON CONFLICT DO NOTHING insert:
// the parent order is not decremented twice, and an already-settled trade is
// not settled again. That makes RecoverTrades safe to run on every startup, and
// safe to run while the stream is live.
//
// Two differences from a streamed trade are worth knowing:
//
//   - A recovered trade is never acked, because acks only exist on the stream.
//     Its client_trades.ack stays FALSE, which is how you tell a pulled trade
//     from a pushed one. If the Core is still holding it unacked it will be
//     redelivered on the next stream connect; that redelivery is a no-op.
//   - A trade whose parent order is not in client_orders cannot be enriched, so
//     it is counted in Failed and left unstored. That is a real inconsistency
//     and needs looking at — the SDK writes the order before it is ever sent to
//     the Core, so a trade against an unknown order should not happen.
//
// The handler MUST be idempotent, for the same reason it must be on the stream:
// a trade may be presented to it more than once.
//
// A non-nil error means the window was not fully processed; the returned
// RecoverResult still reports everything done up to that point.
func (c *Client) RecoverTrades(ctx context.Context, p *RecoverParams, handler TradeEventHandler) (*RecoverResult, error) {
	if p == nil || p.PartnerId == "" {
		return nil, ErrPartnerIDRequired
	}
	days, err := p.days()
	if err != nil {
		return nil, err
	}

	res := &RecoverResult{Days: days}
	for _, day := range days {
		// GetTrades is scoped to one trading day, so a multi-day gap is one
		// call per day, oldest first.
		start, err := time.Parse(dayLayout, day)
		if err != nil {
			return res, fmt.Errorf("%w: %q", ErrInvalidDay, day)
		}
		// Partner scope: recovery catches up one partner's own books, unlike
		// the reconciliation path which has to cover the whole SDK key.
		raw, err := c.getTradesRaw(ctx, p.PartnerId, day,
			start.Format(timestampLayout), start.AddDate(0, 0, 1).Format(timestampLayout), false)
		if err != nil {
			return res, err
		}
		res.CoreTrades += len(raw)

		for _, t := range raw {
			event, inserted, settled, perr := c.persistTrade(ctx, t)
			if perr != nil {
				log.Printf("fx-sdk: recover trade trade_id=%d day=%s order_id=%d: %v\n",
					t.GetId(), t.GetTradingDay(), t.GetOrderId(), perr)
				res.Failed++
				continue
			}
			if inserted {
				res.Recovered++
			}
			// Settle whatever is not settled yet, whether this call stored it
			// or a previous run did and its settlement failed.
			if settled {
				continue
			}
			if serr := c.settle(ctx, handler, event); serr != nil {
				res.SettleFailed++
				continue
			}
			res.Settled++
		}
	}
	return res, nil
}

// days expands the window into the YYYY-MM-DD days to pull, oldest first. DayTo
// is inclusive and defaults to DayFrom, so a single day is the common case.
func (p *RecoverParams) days() ([]string, error) {
	from, err := time.Parse(dayLayout, p.DayFrom)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDay, p.DayFrom)
	}
	to := from
	if p.DayTo != "" {
		if to, err = time.Parse(dayLayout, p.DayTo); err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidDay, p.DayTo)
		}
	}
	if to.Before(from) {
		return nil, fmt.Errorf("%w: %s is before %s", ErrInvalidRecoverRange, p.DayTo, p.DayFrom)
	}

	var days []string
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		if len(days) == maxRecoverDays {
			return nil, fmt.Errorf("%w: %d days, max %d", ErrInvalidRecoverRange,
				int(to.Sub(from).Hours()/24)+1, maxRecoverDays)
		}
		days = append(days, d.Format(dayLayout))
	}
	return days, nil
}
