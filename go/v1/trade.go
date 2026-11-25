package v1

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alifcapital/fx-sdk/go/forexv1"
	"github.com/jackc/pgx/v5"
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
// The call blocks until ctx is cancelled or the stream returns a non-retryable
// error. A subscription is long-lived, so transient gRPC errors and clean
// server-side EOFs trigger a backoff-and-reconnect indefinitely (maxRetries
// bounds per-call unary retries, not the lifetime of a stream).
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
// trades until a Recv or Send error. It resets *attempt once the server accepts
// the stream, so a connection that re-establishes successfully starts its next
// backoff from the base delay.
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
	// Trade opens the stream lazily; Header() blocks until the server actually
	// accepts it. Reset the reconnect counter only once the connection is
	// established, so backoff keeps growing if the open fails.
	if _, err := stream.Header(); err != nil {
		return err
	}
	*attempt = 0
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}

		event, _, settled, perr := c.persistTrade(ctx, resp)
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
//
// The handler's error is logged and recorded before it is returned, so callers
// that only drive the trade forward — the stream and RetryUnsettled — can
// ignore it.
func (c *Client) settle(ctx context.Context, handler TradeEventHandler, event *TradeEvent) error {
	if handler == nil {
		c.markSettled(ctx, event)
		return nil
	}
	if herr := handler(ctx, event); herr != nil {
		log.Printf("fx-sdk: settle trade_id=%d day=%s: %v\n", event.TradeId, event.TradingDay, herr)
		if _, err := c.db.Exec(ctx,
			`UPDATE client_trades SET settle_attempts = settle_attempts + 1, settle_error = $1, updated_at = NOW()
			  WHERE trading_day = $2 AND trade_id = $3 AND order_id = $4`,
			herr.Error(), event.TradingDay, event.TradeId, event.OrderId); err != nil {
			log.Printf("fx-sdk: record settle failure trade_id=%d: %v\n", event.TradeId, err)
		}
		return herr
	}
	c.markSettled(ctx, event)
	return nil
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
		        filled_quantity::text, execution_rate::text, partner_id, client_id, trade_type
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
			&ev.FilledQuantity, &ev.ExecutionRate, &ev.PartnerId, &ev.ClientId, &ev.TradeType); err != nil {
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
// It returns the enriched TradeEvent, whether this call is what stored the
// trade (false on a redelivery of a trade that was already present) and whether
// the trade is already settled.
func (c *Client) persistTrade(ctx context.Context, resp *forexv1.TradeResponse) (event *TradeEvent, inserted, settled bool, err error) {
	// The trade insert and the parent-order update share one transaction, so
	// the trades row and the orders row stay consistent if either fails.
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return nil, false, false, fmt.Errorf("begin trade tx: %w", err)
	}
	defer tx.Rollback(ctx)

	event, inserted, settled, err = persistTradeTx(ctx, tx, resp)
	if err != nil {
		return nil, false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, false, fmt.Errorf("commit trade tx: %w", err)
	}
	return event, inserted, settled, nil
}

// persistTradeTx is persistTrade inside a transaction the caller owns and
// commits, so ReconcileOrders can store an order's missing trades and correct
// the order itself in one transaction.
func persistTradeTx(ctx context.Context, tx pgx.Tx, resp *forexv1.TradeResponse) (event *TradeEvent, inserted, settled bool, err error) {
	event = &TradeEvent{
		PartnerId:      resp.GetPartnerId(),
		TradeId:        resp.GetId(),
		OrderId:        resp.GetOrderId(),
		OrderStatus:    OrderStatus(resp.GetOrderStatus()),
		TradingDay:     resp.GetTradingDay(),
		FilledQuantity: resp.GetFilledQuantity(),
		ExecutionRate:  resp.GetExecutionRate(),
		ExecutedAt:     resp.GetExecutedAt(),
		TradeType:      TradeType(resp.GetTradeType()),
	}

	if err := tx.QueryRow(ctx,
		`SELECT client_id, side, currency_pair, account, fee FROM client_orders WHERE order_day = $1 AND ref_id = $2`,
		resp.GetOrderDay(), resp.GetRefId(),
	).Scan(&event.ClientId, &event.Side, &event.CurrencyPair, &event.Account, &event.FeeConfig); err != nil {
		return nil, false, false, fmt.Errorf("lookup order %d: %w", resp.GetRefId(), err)
	}
	//calc fee
	if err := event.Cal(); err != nil {
		return nil, false, false, fmt.Errorf("compute fee: %w", err)
	}
	// executed_at must be the Core's execution time, never the moment this row
	// was written: a trade stored by ReconcileOrders a day later still belongs
	// to the hour it was executed in.
	//
	// A value that will not parse falls back to the column default rather than
	// rejecting the trade: storing it late is recoverable, dropping it is not.
	var executedAt *time.Time
	if t, perr := time.Parse(time.RFC3339, resp.GetExecutedAt()); perr == nil {
		executedAt = &t
	} else if resp.GetExecutedAt() != "" {
		log.Printf("fx-sdk: unparsable executed_at %q trade_id=%d: %v\n", resp.GetExecutedAt(), event.TradeId, perr)
	}

	cmd, err := tx.Exec(ctx,
		`INSERT INTO client_trades (executed_at, trading_day, trade_id, order_id, ref_id, side, filled_quantity,
		                            execution_rate, settlement, fee, partner_id, client_id, trade_type)
		 VALUES (COALESCE($1, NOW()), $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 ON CONFLICT (trade_id, order_id, trading_day) DO NOTHING;`,
		executedAt, event.TradingDay, event.TradeId, event.OrderId, resp.GetRefId(), event.Side, event.FilledQuantity, event.ExecutionRate,
		event.Settlement, event.Fee, event.PartnerId, event.ClientId, event.TradeType)
	if err != nil {
		return nil, false, false, fmt.Errorf("insert trade: %w", err)
	}

	if cmd.RowsAffected() == 0 {
		// Redelivery: the trade is already stored and the parent order was
		// already decremented on first delivery. Do not touch the order again;
		// report the current settled state so the caller can decide whether to
		// re-run settlement.
		if err := tx.QueryRow(ctx,
			`SELECT settled FROM client_trades WHERE trading_day = $1 AND trade_id = $2 AND order_id = $3`,
			event.TradingDay, event.TradeId, event.OrderId).Scan(&settled); err != nil {
			return nil, false, false, fmt.Errorf("read settled state: %w", err)
		}
		return event, false, settled, nil
	}

	// First delivery: apply the parent-order decrement exactly once, guarded by
	// the trade insert above.
	//
	// A trade moves an order only to FilledPartially, or to Filled with nothing
	// left; closing statuses — expired, cancelled — come from order events. The
	// trade's own order status is a snapshot taken when the trade was settled,
	// which for a market order is already CancelledPartially, so taking it as
	// it is would close the order on its first trade and skip the decrement of
	// the next.
	//
	// An order already closed here is left alone: its status and remaining
	// came from the Core (an order event, ReconcileOrders) or from the trade
	// that filled it, and they account for this trade already. The event that
	// cancels a market order's remainder is sent at once, while the trades go
	// through settlement and arrive after it, so decrementing here would count
	// every one of them twice.
	cmd, err = tx.Exec(ctx,
		`UPDATE client_orders
		    SET status = CASE WHEN status IN ($5, $6, $7, $8, $9) THEN status
		                      WHEN $1 = $5 THEN $5
		                      ELSE $10 END,
		        remaining_quantity = CASE WHEN status IN ($5, $6, $7, $8, $9) THEN remaining_quantity
		                                  WHEN $1 = $5 THEN 0
		                                  ELSE GREATEST(remaining_quantity - $2::numeric, 0) END,
		        updated_at = NOW()
		  WHERE order_day = $3 AND ref_id = $4`,
		int16(event.OrderStatus), event.FilledQuantity, resp.GetOrderDay(), resp.GetRefId(),
		int16(Filled), int16(Cancelled), int16(Expired), int16(CancelledPartially), int16(ExpiredPartially),
		int16(FilledPartially))
	if err != nil {
		return nil, false, false, fmt.Errorf("update order: %w", err)
	}
	if cmd.RowsAffected() != 1 {
		return nil, false, false, fmt.Errorf("expected 1 row affected, got %d", cmd.RowsAffected())
	}
	return event, true, false, nil
}

const (
	// dayLayout is the YYYY-MM-DD form used for every date on the wire and in
	// the local tables.
	dayLayout = "2006-01-02"
	// timestampLayout is the form the Core expects for the dt_from / dt_to
	// bounds of a GetTrades window.
	timestampLayout = "2006-01-02 15:04:05"
)

// GetTrades returns the trades the Core recorded for the partner in a time
// window. It is a read-only query against the Core's own books — unlike the
// trade stream it stores nothing, acks nothing and settles nothing, so it is
// safe to call at any time.
//
// Use it to read the Core's trade list, e.g. over a whole day, for reports or a
// manual check against client_trades.
//
// DtFrom and DtTo default to the full Day when left empty. The bounds are
// interpreted by the Core in its own timezone (UTC+05:00).
func (c *Client) GetTrades(ctx context.Context, p *GetTradesParams) ([]Trade, error) {
	if p == nil || p.PartnerId == "" {
		return nil, ErrPartnerIDRequired
	}
	day, err := time.Parse(dayLayout, p.Day)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDay, p.Day)
	}
	dtFrom, dtTo := p.DtFrom, p.DtTo
	if dtFrom == "" {
		dtFrom = day.Format(timestampLayout)
	}
	if dtTo == "" {
		dtTo = day.AddDate(0, 0, 1).Format(timestampLayout)
	}

	raw, err := c.getTradesRaw(ctx, p.PartnerId, p.Day, dtFrom, dtTo)
	if err != nil {
		return nil, err
	}
	return toTrades(raw), nil
}

// getTradesRaw performs the GetTrades call for partnerId's own trades and
// returns the Core's messages untouched.
//
// The window is read a page at a time and returned whole. One response used to
// carry all of it, and a busy day did not fit into a single gRPC message. A Core that
// predates paging ignores the page fields and answers with everything and no
// token, which ends the loop after one round, so this works against either.
func (c *Client) getTradesRaw(ctx context.Context, partnerId, day, dtFrom, dtTo string) ([]*forexv1.TradeResponse, error) {
	var (
		trades []*forexv1.TradeResponse
		token  string
	)
	pageSize := int32(tradesPageSize)
	for page := 0; ; page++ {
		req := &forexv1.GetTradesRequest{
			TradingDay: &day,
			DtFrom:     &dtFrom,
			DtTo:       &dtTo,
			PartnerId:  &partnerId,
			PageSize:   &pageSize,
		}
		if token != "" {
			req.PageToken = &token
		}
		resp, err := c.trade.GetTrades(ctx, req)
		if err != nil {
			return nil, fmt.Errorf("fx-sdk: get trades %s [%s, %s): %w", day, dtFrom, dtTo, err)
		}
		trades = append(trades, resp.GetTrades()...)

		next := resp.GetNextPageToken()
		if next == "" {
			return trades, nil
		}
		// A token that does not move, or a walk far longer than any real day,
		// means the Core is not paging the way this loop assumes. Stop with an
		// error rather than spin or hand back a list that looks complete.
		if next == token || page >= maxTradePages {
			return nil, fmt.Errorf("fx-sdk: get trades %s [%s, %s): paging did not finish after %d pages",
				day, dtFrom, dtTo, page+1)
		}
		token = next
	}
}

const (
	// tradesPageSize is the Core's own maximum page.
	tradesPageSize = 1000
	// maxTradePages bounds one window at ten million trades.
	maxTradePages = 10000
)

// toTrades projects the Core's messages onto the SDK type. No local enrichment
// happens here — that is persistTrade's job.
func toTrades(raw []*forexv1.TradeResponse) []Trade {
	trades := make([]Trade, 0, len(raw))
	for _, t := range raw {
		trades = append(trades, Trade{
			TradeId:        t.GetId(),
			OrderId:        t.GetOrderId(),
			RefId:          t.GetRefId(),
			OrderStatus:    OrderStatus(t.GetOrderStatus()),
			Side:           Side(t.GetSide()),
			OrderDay:       t.GetOrderDay(),
			TradingDay:     t.GetTradingDay(),
			FilledQuantity: t.GetFilledQuantity(),
			ExecutionRate:  t.GetExecutionRate(),
			ExecutedAt:     t.GetExecutedAt(),
			PartnerId:      t.GetPartnerId(),
			TradeType:      TradeType(t.GetTradeType()),
		})
	}
	return trades
}
