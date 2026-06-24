package v1

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alifcapital/fx-sdk/go/forexv1"
)

// SubscribeTrades opens the bidirectional TradeService stream and drives the
// full trade-settlement flow for every event the Core pushes:
//
//  1. Look up the parent order by ref_id to pick up client_id, side,
//     currency_pair, account and fee configuration.
//  2. Insert the trade into the local trades table.
//  3. Update the order: decrement remaining_quantity by the filled
//     amount and apply the new status reported by the Core.
//  4. Invoke the caller-supplied handler so the partner can run its own
//     account movements (debit/credit) using the enriched event.
//
// The call blocks until ctx is cancelled, the stream returns a non-retryable
// error, or maxRetries consecutive reconnect attempts fail. Transient gRPC
// errors and clean server-side EOFs trigger a backoff-and-reconnect; every
// successful Recv resets the counter, so a healthy stream can survive an
// arbitrary number of blips over its lifetime.
func (c *Client) SubscribeTrades(ctx context.Context, handler TradeEventHandler) error {
	attempt := 0
	for {
		err := c.runTradeStream(ctx, handler, &attempt)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isStreamRetryable(err) {
			return fmt.Errorf("fx-sdk: subscribe trades: %w", err)
		}
		if attempt >= c.maxRetries {
			return fmt.Errorf("fx-sdk: subscribe trades after %d reconnects: %w", attempt, err)
		}
		delay := backoff(attempt, c.baseDelay, c.maxDelay)
		attempt++
		log.Printf("fx-sdk: trade stream disconnected, reconnecting in %s (attempt %d): %v", delay, attempt, err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// runTradeStream opens one bidirectional TradeService stream and processes
// trades until a Recv or Send error. It resets *attempt on every successful
// Recv so transient blips do not burn the reconnect budget.
//
// The flow separates three states that used to be conflated into one ack:
//
//  1. received & stored — persistTrade writes the trade durably. Only after
//     this succeeds do we ack the Core, so the ack means "I have it", not "I
//     settled it". A persist failure skips the ack and the Core redelivers.
//  2. settled — the partner handler (account movement) ran successfully. This
//     is tracked locally on the trades row, independent of the ack.
//
// Redelivery is safe: persistTrade inserts ON CONFLICT DO NOTHING and reports
// whether the trade was already settled, so a redelivered-but-settled trade is
// only re-acked, never re-decremented or re-settled.
func (c *Client) runTradeStream(ctx context.Context, handler TradeEventHandler, attempt *int) error {
	stream, err := c.trade.Trade(ctx)
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		*attempt = 0

		event, settled, perr := c.persistTrade(ctx, resp)
		if perr != nil {
			log.Printf("fx-sdk: persist trade trade_id=%d day=%s order_id=%d: %v\n",
				resp.GetId(), resp.GetTradingDay(), resp.GetOrderId(), perr)
			continue // not stored → no ack → Core redelivers
		}

		// The trade is durably stored. Ack it as received, independent of
		// whether partner-side settlement has completed.
		ack := &forexv1.TradeRequest{
			Side:       resp.Side,
			TradeId:    resp.Id,
			TradingDay: resp.TradingDay,
			PartnerId:  resp.PartnerId,
		}
		if serr := stream.Send(ack); serr != nil {
			return serr
		}
		if _, err := c.db.Exec(ctx,
			"UPDATE client_trades SET ack = TRUE, updated_at = NOW() WHERE trading_day = $1 AND trade_id = $2 AND order_id = $3",
			event.TradingDay, event.TradeId, event.OrderId); err != nil {
			log.Printf("fx-sdk: mark trade ack trade_id=%d: %v\n", event.TradeId, err)
		}

		// Settlement is its own state. Skip if a prior delivery already settled
		// it; otherwise run the handler. A failure leaves settled = FALSE for
		// RetryUnsettled to pick up — it is never lost.
		if !settled {
			c.settle(ctx, handler, event)
		}
	}
}

// settle runs the partner handler for one trade and records the outcome on the
// trades row. On success the row is marked settled; on failure settled stays
// FALSE, the attempt counter is bumped and the error is recorded so
// RetryUnsettled can re-run it later. A nil handler means there is no
// partner-side settlement to perform, so the trade is considered settled.
func (c *Client) settle(ctx context.Context, handler TradeEventHandler, event *TradeEvent) {
	if handler == nil {
		c.markSettled(ctx, event)
		return
	}
	if herr := handler(ctx, event); herr != nil {
		log.Printf("fx-sdk: settle trade_id=%d day=%s: %v\n", event.TradeId, event.TradingDay, herr)
		if _, err := c.db.Exec(ctx,
			`UPDATE client_trades SET settle_attempts = settle_attempts + 1, settle_error = $1, updated_at = NOW()
			  WHERE trading_day = $2 AND trade_id = $3 AND order_id = $4`,
			herr.Error(), event.TradingDay, event.TradeId, event.OrderId); err != nil {
			log.Printf("fx-sdk: record settle failure trade_id=%d: %v\n", event.TradeId, err)
		}
		return
	}
	c.markSettled(ctx, event)
}

// markSettled flags the trade row as settled and clears any prior settle error.
func (c *Client) markSettled(ctx context.Context, event *TradeEvent) {
	if _, err := c.db.Exec(ctx,
		`UPDATE client_trades SET settled = TRUE, settle_error = NULL, updated_at = NOW()
		  WHERE trading_day = $1 AND trade_id = $2 AND order_id = $3`,
		event.TradingDay, event.TradeId, event.OrderId); err != nil {
		log.Printf("fx-sdk: mark settled trade_id=%d: %v\n", event.TradeId, err)
	}
}

// RetryUnsettled re-runs the settlement handler for every trade that was stored
// but not yet settled — e.g. the handler failed, or the process crashed after
// the trade was acked but before settlement completed. The local settled flag,
// not Core redelivery, is the source of truth for "money still needs to move",
// so this is the mechanism that guarantees a failed settlement is eventually
// retried even on a healthy, never-reconnecting stream.
//
// Call it on startup and on a ticker. The handler MUST be idempotent (see
// TradeEventHandler) because a trade may be presented to it more than once.
func (c *Client) RetryUnsettled(ctx context.Context, handler TradeEventHandler) error {
	if handler == nil {
		return nil
	}

	rows, err := c.db.Query(ctx,
		`SELECT to_char(trading_day, 'YYYY-MM-DD'), trade_id, order_id, ref_id,
		        filled_quantity::text, execution_rate::text, partner_id, client_id
		   FROM client_trades
		  WHERE settled = FALSE
		  ORDER BY trading_day`)
	if err != nil {
		return fmt.Errorf("fx-sdk: query unsettled trades: %w", err)
	}

	type pending struct {
		event *TradeEvent
		refId int64
	}
	// Drain the result set before issuing the per-row enrichment queries so we
	// do not hold the cursor's connection open across them. OrderStatus is not
	// persisted on the trade row, so it is left zero here; settlement only needs
	// the fill, side, account and fee config.
	var batch []pending
	for rows.Next() {
		ev := &TradeEvent{}
		var refId int64
		if err := rows.Scan(&ev.TradingDay, &ev.TradeId, &ev.OrderId, &refId,
			&ev.FilledQuantity, &ev.ExecutionRate, &ev.PartnerId, &ev.ClientId); err != nil {
			rows.Close()
			return fmt.Errorf("fx-sdk: scan unsettled trade: %w", err)
		}
		batch = append(batch, pending{event: ev, refId: refId})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("fx-sdk: iterate unsettled trades: %w", err)
	}

	for _, p := range batch {
		// Enrich from the parent order. ref_id is a globally-unique BIGSERIAL,
		// so the lookup is correct without order_day (the hypertable just cannot
		// prune partitions for this background path).
		if err := c.db.QueryRow(ctx,
			`SELECT side, currency_pair, account, fee FROM client_orders WHERE ref_id = $1`,
			p.refId).Scan(&p.event.Side, &p.event.CurrencyPair, &p.event.Account, &p.event.FeeConfig); err != nil {
			log.Printf("fx-sdk: retry settle lookup ref_id=%d: %v\n", p.refId, err)
			continue
		}
		// Recompute settlement/fee from the persisted fill (deterministic).
		if err := p.event.Cal(); err != nil {
			log.Printf("fx-sdk: retry settle compute trade_id=%d: %v\n", p.event.TradeId, err)
			continue
		}
		c.settle(ctx, handler, p.event)
	}
	return nil
}

// persistTrade computes settlement/fee, durably stores the trade and (for a
// first delivery) decrements the parent order. The trade insert is idempotent:
// on redelivery of an already-stored trade the parent order is left untouched
// and the trade's current settled state is returned so the caller can decide
// whether to (re)run settlement.
//
// It returns the enriched TradeEvent and whether the trade was already settled.
func (c *Client) persistTrade(ctx context.Context, resp *forexv1.TradeResponse) (*TradeEvent, bool, error) {

	var (
		event = &TradeEvent{
			PartnerId:      resp.GetPartnerId(),
			TradeId:        resp.GetId(),
			OrderId:        resp.GetOrderId(),
			OrderStatus:    OrderStatus(resp.GetOrderStatus()),
			TradingDay:     resp.GetTradingDay(),
			FilledQuantity: resp.GetFilledQuantity(),
			ExecutionRate:  resp.GetExecutionRate(),
			ExecutedAt:     resp.GetExecutedAt(),
		}
	)

	if err := c.db.QueryRow(ctx,
		`SELECT client_id, side, currency_pair, account, fee FROM client_orders WHERE order_day = $1 AND ref_id = $2`,
		resp.GetOrderDay(), resp.GetRefId(),
	).Scan(&event.ClientId, &event.Side, &event.CurrencyPair, &event.Account, &event.FeeConfig); err != nil {
		return nil, false, fmt.Errorf("lookup order %d: %w", resp.GetRefId(), err)
	}
	//calc fee
	if err := event.Cal(); err != nil {
		return nil, false, fmt.Errorf("compute fee: %w", err)
	}
	// Wrap the trade insert and parent-order update in a single transaction so
	// the trades row and orders row stay consistent if either statement fails.
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin trade tx: %w", err)
	}
	defer tx.Rollback(ctx)

	cmd, err := tx.Exec(ctx,
		`INSERT INTO client_trades (trading_day, trade_id, order_id, ref_id, side, filled_quantity,
		                            execution_rate, settlement, fee, partner_id, client_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (trade_id, order_id, trading_day) DO NOTHING;`,
		event.TradingDay, event.TradeId, event.OrderId, resp.GetRefId(), event.Side, event.FilledQuantity, event.ExecutionRate,
		event.Settlement, event.Fee, event.PartnerId, event.ClientId)
	if err != nil {
		return nil, false, fmt.Errorf("insert trade: %w", err)
	}

	if cmd.RowsAffected() == 0 {
		// Redelivery: the trade is already stored and the parent order was
		// already decremented on first delivery. Do not touch the order again;
		// report the current settled state so the caller can decide whether to
		// re-run settlement.
		var settled bool
		if err := tx.QueryRow(ctx,
			`SELECT settled FROM client_trades WHERE trading_day = $1 AND trade_id = $2 AND order_id = $3`,
			event.TradingDay, event.TradeId, event.OrderId).Scan(&settled); err != nil {
			return nil, false, fmt.Errorf("read settled state: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, false, fmt.Errorf("commit trade tx: %w", err)
		}
		return event, settled, nil
	}

	// First delivery: apply the parent-order decrement exactly once, guarded by
	// the trade insert above.
	cmd, err = tx.Exec(ctx,
		`UPDATE client_orders SET status  = $1,
		        remaining_quantity        = GREATEST(remaining_quantity - $2::numeric, 0),
		        updated_at                = NOW()
		  WHERE order_day = $3 AND ref_id = $4`,
		event.OrderStatus, event.FilledQuantity, resp.GetOrderDay(), resp.GetRefId())
	if err != nil {
		return nil, false, fmt.Errorf("update order: %w", err)
	}
	if cmd.RowsAffected() != 1 {
		return nil, false, fmt.Errorf("expected 1 row affected, got %d", cmd.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit trade tx: %w", err)
	}

	return event, false, nil
}
