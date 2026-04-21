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
// NOTE on at-least-once delivery: if a reconnect happens after a trade was
// persisted locally but before the ack reached the Core, the Core will
// redeliver the same (trade_id, trading_day). The current INSERT into
// client_trades will then fail on the primary key, which persistTrade surfaces
// as an error and we ack "failed" — fixing that duplicate handling is a
// separate change.
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
		event, perr := c.persistTrade(ctx, resp)
		if perr != nil {
			log.Printf("fx-sdk: persist trade trade_id=%d day=%s, order_id=%d: %v\n", resp.GetId(), resp.GetTradingDay(), resp.GetOrderId(), perr)
			continue
		} else if handler != nil {
			if herr := handler(ctx, event); herr != nil {
				log.Printf("fx-sdk: trade handler trade_id=%d: %v\n", event.TradeID, herr)
			}
		}

		ack := &forexv1.TradeRequest{
			Side:       resp.Side,
			TradeId:    resp.Id,
			TradingDay: resp.TradingDay,
		}
		if serr := stream.Send(ack); serr != nil {
			return serr
		}
		_, err = c.db.Exec(ctx,
			"UPDATE client_trades SET ack = TRUE, updated_at = NOW() WHERE trading_day = $1 AND trade_id = $2 AND order_id = $3",
			event.TradingDay, event.TradeID, resp.GetOrderId())
		if err != nil {
			log.Printf("fx-sdk: mark trade ack trade_id=%d: %v\n", event.TradeID, err)
		}
	}
}

// persistTrade computes settlement/fee, writes the trade row, updates the
// parent order, and returns an enriched TradeEvent for the caller.
func (c *Client) persistTrade(ctx context.Context, resp *forexv1.TradeResponse) (*TradeEvent, error) {

	var (
		event = &TradeEvent{
			TradeID:        resp.GetId(),
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
	).Scan(&event.ClientID, &event.Side, &event.CurrencyPair, &event.Account, &event.FeeConfig); err != nil {
		return nil, fmt.Errorf("lookup order %d: %w", resp.GetRefId(), err)
	}
	//calc fee
	if err := event.Cal(); err != nil {
		return nil, fmt.Errorf("compute fee: %w", err)
	}
	// Wrap the trade insert and parent-order update in a single transaction so
	// the trades row and orders row stay consistent if either statement fails.
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin trade tx: %w", err)
	}
	defer tx.Rollback(ctx)

	cmd, err := tx.Exec(ctx,
		`INSERT INTO client_trades (trading_day, trade_id, order_id, filled_quantity,
		                            execution_rate, settlement, fee, client_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8);`,
		event.TradingDay, event.TradeID, resp.GetOrderId(), event.FilledQuantity, event.ExecutionRate,
		event.Settlement, event.Fee, event.ClientID)
	if err != nil {
		return nil, fmt.Errorf("insert trade: %w", err)
	}
	if cmd.RowsAffected() != 1 {
		return nil, fmt.Errorf("expected 1 row affected, got %d", cmd.RowsAffected())
	}
	cmd, err = tx.Exec(ctx,
		`UPDATE client_orders SET status  = $1,
		        remaining_quantity        = GREATEST(remaining_quantity - $2::numeric, 0),
		        updated_at                = NOW()
		  WHERE order_day = $3 AND ref_id = $4`,
		event.OrderStatus, event.FilledQuantity, resp.GetOrderDay(), resp.GetRefId())
	if err != nil {
		return nil, fmt.Errorf("update order: %w", err)
	}
	if cmd.RowsAffected() != 1 {
		return nil, fmt.Errorf("expected 1 row affected, got %d", cmd.RowsAffected())
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit trade tx: %w", err)
	}

	return event, nil
}
