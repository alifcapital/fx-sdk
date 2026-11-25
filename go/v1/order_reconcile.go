package v1

import (
	"context"
	"fmt"
	"log"
	"time"

	forexv1 "github.com/alifcapital/fx-sdk/go/forexv1"
)

// orderStatusBatch is how many orders one GetOrderStatuses call asks about. It
// is the Core's own limit per request.
const orderStatusBatch = 500

const (
	// defaultOrderOlderThan is past the longest order lifetime (a retail order
	// lives 24 hours; corporate and treasury orders end at 18:00 the same day)
	// with an hour to spare, so every order it selects is closed in the Core.
	defaultOrderOlderThan = 25 * time.Hour
	defaultOrderLookback  = 7
)

// CauseNotInCore is stored as client_orders.cause for an order the Core has no
// record of, long after it was submitted: it never reached the Core.
const CauseNotInCore = "ордер не дошёл до Core"

// ReconcileOrdersParams configures ReconcileOrders. Only PartnerId is required.
type ReconcileOrdersParams struct {
	PartnerId string
	// OlderThan selects orders submitted at least this long ago. Default 25
	// hours: past every order's lifetime, so the Core's answer is final.
	OlderThan time.Duration
	// LookbackDays is how many order days back, today included, are looked at.
	// Default 7, so an application that was down for days still catches up.
	LookbackDays int
}

// ReconcileOrdersResult reports what one ReconcileOrders run did.
type ReconcileOrdersResult struct {
	Checked     int // live local orders asked about
	TradesAdded int // trades the Core had and this SDK did not: stored and passed to the trade handler
	Closed      int // orders closed with the Core's status
	Completed   int // closed here already but short of trades: the missing trades stored, nothing else changed
	Released    int // of those, orders whose blocked funds were released through the order handler
	Corrected   int // closed without a release: it had already happened (see ReconcileOrders)
	Lost        int // not in the Core at all: marked Failed, funds released
	StillLive   int // still live in the Core (its expiry has not run yet): left for the next run
	Behind      int // the Core reports more filled than the trades it returned: left for the next run
}

// ReconcileOrders is the SDK's one reconciliation. It finds local orders whose
// lifetime is over and that are either still Pending or FilledPartially, or
// closed but holding fewer trades than they filled (quantity − remaining is
// more than the trades stored for them: the order event arrived, some trades
// did not). It asks the Core what became of them and brings each one — its
// trades and its own row — into line in a single transaction.
//
// A closed order only gets its missing trades: its status and remaining came
// from the Core already, and its funds were released when it closed, so the
// order handler is not called for it.
//
// For every such order:
//
//  1. Trades the Core recorded for it and this SDK did not are stored exactly
//     as the trade stream stores them, decrementing the order's remaining.
//  2. The order's remaining is then compared with the Core's:
//     - equal: no order event was ever applied here, so none of its funds was
//     released. The order takes the Core's status, and for Expired,
//     ExpiredPartially, Cancelled, CancelledPartially and Failed the order
//     handler is called with the Core's remaining — the same OrderEvent the
//     stream would have delivered.
//     - lower here: the order event did arrive and was handled, then a trade
//     reached this SDK after it and decremented the remaining a second time.
//     The order takes the Core's status and remaining; the funds were already
//     released, so the order handler is not called.
//     - higher here: the Core reports fills whose trades it has not returned
//     yet (their settlement is still being recorded). The order is left as it
//     is and the next run tries again.
//  3. An order the Core has never heard of never reached it: it is marked
//     Failed with CauseNotInCore and the order handler releases its remaining.
//
// After the transaction commits, the trade handler runs for every stored trade
// not yet settled (the money movement), then the order handler for a release.
// Run it at start-up and hourly. It never touches orders that are not live,
// and an order closed meanwhile by the stream is skipped.
func (c *Client) ReconcileOrders(ctx context.Context, p *ReconcileOrdersParams, tradeHandler TradeEventHandler, orderHandler OrderEventHandler) (*ReconcileOrdersResult, error) {
	if p == nil || p.PartnerId == "" {
		return nil, ErrPartnerIDRequired
	}
	olderThan, lookback := p.OlderThan, p.LookbackDays
	if olderThan <= 0 {
		olderThan = defaultOrderOlderThan
	}
	if lookback <= 0 {
		lookback = defaultOrderLookback
	}

	res := &ReconcileOrdersResult{}
	// Keyset over (order_day, ref_id): orders leave the live set during the
	// run, which would shift an offset.
	afterDay, afterRef := "0001-01-01", int64(0)
	for {
		batch, err := c.staleLiveOrders(ctx, p.PartnerId, olderThan, lookback, afterDay, afterRef)
		if err != nil {
			return res, err
		}
		if len(batch) == 0 {
			return res, nil
		}
		last := batch[len(batch)-1]
		afterDay, afterRef = last.day, last.refId

		if err := c.reconcileOrderBatch(ctx, p.PartnerId, batch, tradeHandler, orderHandler, res); err != nil {
			return res, err
		}
		if len(batch) < orderStatusBatch {
			return res, nil
		}
	}
}

type staleOrder struct {
	day   string
	refId int64
}

// staleLiveOrders reads the next batch of the partner's orders submitted more
// than olderThan ago that need the Core — live ones, and closed ones whose
// filled quantity is not covered by their stored trades — oldest key first,
// after (afterDay, afterRef).
func (c *Client) staleLiveOrders(ctx context.Context, partnerId string, olderThan time.Duration, lookback int, afterDay string, afterRef int64) ([]staleOrder, error) {
	rows, err := c.db.Query(ctx, `
		SELECT o.order_day::text, o.ref_id
		  FROM client_orders o
		 WHERE o.partner_id = $1
		   AND o.order_day >= (NOW() AT TIME ZONE INTERVAL '+05:00')::date - $4::int
		   AND o.submitted_at < NOW() - $5::interval
		   AND (o.order_day, o.ref_id) > ($6::date, $7::bigint)
		   AND (o.status IN ($2, $3)
		        OR (o.status IN ($9, $10, $11, $12, $13)
		            AND o.quantity - o.remaining_quantity > COALESCE((
		                SELECT sum(t.filled_quantity) FROM client_trades t
		                 WHERE t.ref_id = o.ref_id
		                   AND t.trading_day BETWEEN o.order_day AND o.order_day + 1), 0)))
		 ORDER BY o.order_day, o.ref_id
		 LIMIT $8`,
		partnerId, int16(Pending), int16(FilledPartially), lookback-1, olderThan, afterDay, afterRef, orderStatusBatch,
		int16(Filled), int16(Cancelled), int16(Expired), int16(CancelledPartially), int16(ExpiredPartially))
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: read orders to reconcile: %w", err)
	}
	defer rows.Close()

	var out []staleOrder
	for rows.Next() {
		var o staleOrder
		if err := rows.Scan(&o.day, &o.refId); err != nil {
			return nil, fmt.Errorf("fx-sdk: scan order to reconcile: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (c *Client) reconcileOrderBatch(ctx context.Context, partnerId string, batch []staleOrder, tradeHandler TradeEventHandler, orderHandler OrderEventHandler, res *ReconcileOrdersResult) error {
	keys := make([]*forexv1.OrderKey, len(batch))
	for i, o := range batch {
		keys[i] = &forexv1.OrderKey{OrderDay: &o.day, RefId: &o.refId}
	}
	resp, err := c.order.GetOrderStatuses(ctx, &forexv1.GetOrderStatusesRequest{
		PartnerId: &partnerId,
		Keys:      keys,
	})
	if err != nil {
		return fmt.Errorf("fx-sdk: get order statuses: %w", err)
	}
	res.Checked += len(batch)

	type key struct {
		day string
		ref int64
	}
	remote := make(map[key]*forexv1.OrderStatusInfo, len(resp.GetOrders()))
	for _, o := range resp.GetOrders() {
		remote[key{o.GetOrderDay(), o.GetRefId()}] = o
	}

	for _, o := range batch {
		out, err := c.reconcileOrder(ctx, o, remote[key{o.day, o.refId}])
		if err != nil {
			// One order failing does not stop the rest; it is picked up again
			// by the next run.
			log.Printf("fx-sdk: reconcile order %s/%d: %v\n", o.day, o.refId, err)
			continue
		}
		res.TradesAdded += out.tradesAdded
		switch out.kind {
		case outcomeClosed:
			res.Closed++
		case outcomeCorrected:
			res.Closed++
			res.Corrected++
		case outcomeCompleted:
			res.Completed++
		case outcomeLost:
			res.Lost++
		case outcomeStillLive:
			res.StillLive++
		case outcomeBehind:
			res.Behind++
			log.Printf("fx-sdk: reconcile order %s/%d: the Core reports fills it has not returned yet; retrying next run\n", o.day, o.refId)
		}

		// After the commit: the money movement for every unsettled trade, then
		// the release.
		for _, ev := range out.unsettled {
			_ = c.settle(ctx, tradeHandler, ev)
		}
		if out.release != nil && orderHandler != nil {
			res.Released++
			orderHandler(out.release)
		}
	}
	return nil
}

type orderOutcome int

const (
	outcomeSkipped   orderOutcome = iota // closed meanwhile by the stream
	outcomeClosed                        // took the Core's status; any release is in release
	outcomeCorrected                     // took the Core's status; funds already released earlier
	outcomeLost                          // not in the Core: Failed
	outcomeStillLive                     // live in the Core too
	outcomeBehind                        // Core fills not all returned yet
	outcomeCompleted                     // closed here already: only its missing trades stored
)

type reconciled struct {
	kind        orderOutcome
	tradesAdded int
	unsettled   []*TradeEvent
	release     *OrderEvent
}

// reconcileOrder brings one order and its trades into line with the Core in a
// single transaction; see ReconcileOrders for the rules. r is nil or not found
// when the Core has no such order.
func (c *Client) reconcileOrder(ctx context.Context, o staleOrder, r *forexv1.OrderStatusInfo) (reconciled, error) {
	var out reconciled
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)

	// Lock the row and re-check it is still live: an event from the stream may
	// have closed it since the batch was read, and then it is not ours to touch.
	var (
		status    int16
		remaining string
		partnerId string
	)
	err = tx.QueryRow(ctx, `
		SELECT status, remaining_quantity::text, partner_id FROM client_orders
		 WHERE order_day = $1 AND ref_id = $2 FOR UPDATE`, o.day, o.refId,
	).Scan(&status, &remaining, &partnerId)
	if err != nil {
		return out, fmt.Errorf("lock order: %w", err)
	}
	live := OrderStatus(status) == Pending || OrderStatus(status) == FilledPartially
	if !live {
		// Closed here: by an order event, or by the trade that filled it. Its
		// status and remaining are the Core's and its funds were released, so
		// only the trades it is missing are stored — persistTradeTx leaves a
		// closed order's row as it is.
		if r == nil || !r.GetFound() {
			return out, nil // outcomeSkipped
		}
		for _, t := range r.GetTrades() {
			ev, inserted, settled, err := persistTradeTx(ctx, tx, t)
			if err != nil {
				return reconciled{}, fmt.Errorf("store trade %d: %w", t.GetId(), err)
			}
			if inserted {
				out.tradesAdded++
			}
			if !settled {
				out.unsettled = append(out.unsettled, ev)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return reconciled{}, err
		}
		if out.tradesAdded > 0 {
			out.kind = outcomeCompleted
		}
		return out, nil
	}

	if r == nil || !r.GetFound() {
		if _, err := tx.Exec(ctx, `
			UPDATE client_orders SET status = $1, cause = $2, updated_at = NOW()
			 WHERE order_day = $3 AND ref_id = $4`,
			int16(Failed), CauseNotInCore, o.day, o.refId); err != nil {
			return out, fmt.Errorf("mark lost: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return out, err
		}
		out.kind = outcomeLost
		// Nothing of it ever traded, so everything left here is what was reserved.
		out.release = orderEvent(o, Failed, remaining, partnerId)
		return out, nil
	}

	// 1. The trades this SDK is missing, stored the way the stream stores them.
	for _, t := range r.GetTrades() {
		ev, inserted, settled, err := persistTradeTx(ctx, tx, t)
		if err != nil {
			return reconciled{}, fmt.Errorf("store trade %d: %w", t.GetId(), err)
		}
		if inserted {
			out.tradesAdded++
		}
		if !settled {
			out.unsettled = append(out.unsettled, ev)
		}
	}

	st := OrderStatus(r.GetStatus())
	if !isFinal(st) {
		if err := tx.Commit(ctx); err != nil {
			return reconciled{}, err
		}
		out.kind = outcomeStillLive
		return out, nil
	}

	// 2. Compare what is left here with the Core, after the trades above.
	var cmp int
	if err := tx.QueryRow(ctx, `
		SELECT sign(remaining_quantity - $1::numeric)::int FROM client_orders
		 WHERE order_day = $2 AND ref_id = $3`, r.GetRemainingQuantity(), o.day, o.refId,
	).Scan(&cmp); err != nil {
		return reconciled{}, fmt.Errorf("compare remaining: %w", err)
	}
	if cmp > 0 {
		if err := tx.Commit(ctx); err != nil {
			return reconciled{}, err
		}
		out.kind = outcomeBehind
		return out, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE client_orders
		   SET status             = $1,
		       remaining_quantity = $2::numeric,
		       order_id           = COALESCE(order_id, NULLIF($3::bigint, 0)),
		       updated_at         = NOW()
		 WHERE order_day = $4 AND ref_id = $5`,
		int16(st), r.GetRemainingQuantity(), r.GetOrderId(), o.day, o.refId); err != nil {
		return reconciled{}, fmt.Errorf("close order: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return reconciled{}, err
	}

	if cmp < 0 {
		// A trade decremented this order after its event had set the Core's
		// remaining: the event was handled, and so was the release.
		out.kind = outcomeCorrected
		return out, nil
	}
	out.kind = outcomeClosed
	if releasable(st) {
		out.release = orderEvent(o, st, r.GetRemainingQuantity(), partnerId)
	}
	return out, nil
}

func orderEvent(o staleOrder, st OrderStatus, remaining, partnerId string) *OrderEvent {
	return &OrderEvent{
		RefId:             o.refId,
		OrderDay:          o.day,
		EventType:         st,
		EventTimestamp:    time.Now().Format(time.RFC3339),
		RemainingQuantity: remaining,
		PartnerId:         partnerId,
	}
}

func isFinal(st OrderStatus) bool {
	switch st {
	case Filled, Cancelled, Expired, Failed, CancelledPartially, ExpiredPartially, Rejected, Duplicate:
		return true
	}
	return false
}

// releasable reports whether closing an order in this status frees what was
// left of its reserved funds.
func releasable(st OrderStatus) bool {
	switch st {
	case Cancelled, Expired, Failed, CancelledPartially, ExpiredPartially:
		return true
	}
	return false
}
