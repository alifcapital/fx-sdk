package v1

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/alifcapital/fx-sdk/go/forexv1"
)

// SubmitOrder inserts the order into the local database, reads back the
// generated BIGSERIAL id and order_day, stringifies the id as ref_id, then
// forwards the order to the Core. The local row is updated with the Core's
// response status. The composite PK (id, order_day) is used on update so the
// TimescaleDB hypertable can prune to a single partition.
func (c *Client) SubmitOrder(ctx context.Context, p *SubmitOrderParams) (*SubmitOrderResult, error) {

	if p == nil || p.ClientID == "" {
		return nil, ErrClientIDRequired
	}

	if p.Segment < Retail || p.Segment > Treasury {
		return nil, fmt.Errorf("fx-sdk: SubmitOrder: segment is required")
	}
	// 0. Check for duplicates in the last 2 minutes.
	// Scoped to today's partition so the hypertable only scans a single chunk.
	var exists bool
	err := c.db.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM client_orders
			WHERE order_day = CURRENT_DATE
			  AND submitted_at > NOW() - INTERVAL '2 minutes'
			  AND client_id = $3
			  AND side = $1
			  AND limit_rate = $2
			  AND quantity = $4
			  AND currency_pair = $5);`, p.Side, p.LimitRate, p.ClientID, p.Quantity, p.CurrencyPair).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: check duplicate: %w", err)
	}
	if exists {
		return nil, ErrDuplicateOrder
	}

	// 1. Insert into client_orders; the DB assigns ref_id (BIGSERIAL) and
	// order_day (DEFAULT CURRENT_DATE). Read both back so subsequent updates
	// can target the composite PK.
	var (
		refId    int64
		orderDay string
	)
	err = c.db.QueryRow(ctx,
		`INSERT INTO client_orders (side, segment, quantity, limit_rate, remaining_quantity,
		                            min_trade_quantity, allow_partial_fill, currency_pair,
		                            client_id, client_inn, account, fee)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING ref_id, to_char(order_day, 'YYYY-MM-DD');`,
		p.Side, p.Segment,
		p.Quantity, p.LimitRate, p.Quantity, // remaining_quantity starts equal to quantity
		p.MinTradeQuantity,
		p.AllowPartialFill,
		p.CurrencyPair, p.ClientID, p.ClientINN,
		p.Account, p.Fee,
	).Scan(&refId, &orderDay)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: insert order: %w", err)
	}

	// 2. Build and send the gRPC request. ref_id is the local BIGSERIAL,
	// carried over the wire as int64.
	req := &forexv1.SubmitOrderRequest{
		Side:             new(int32(p.Side)),
		Segment:          new(int32(p.Segment)),
		AllowPartialFill: new(p.AllowPartialFill),
		ClientId:         &p.ClientID,
		ClientInn:        &p.ClientINN,
		CurrencyPair:     &p.CurrencyPair,
		Quantity:         &p.Quantity,
		LimitRate:        &p.LimitRate,
		RefId:            &refId,
		OrderDay:         &orderDay,
		MinTradeQuantity: &p.MinTradeQuantity,
	}

	resp, err := c.order.SubmitOrder(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: submit order: %w", err)
	}

	// 3. Reflect the Core's response into the local row.
	orderStatus := OrderStatus(resp.GetStatus())

	switch orderStatus {
	case Pending, Duplicate, Unknown:
		return &SubmitOrderResult{
			RefID:    refId,
			OrderDay: orderDay,
			Status:   orderStatus,
		}, nil
	default:
	}

	cmd, err := c.db.Exec(ctx,
		`UPDATE client_orders SET status = $1, cause = $2, updated_at = NOW()
		  WHERE ref_id = $3 AND order_day = $4`, orderStatus, resp.GetCause(), refId, orderDay)
	if err != nil {
		log.Println("fx-sdk: submit order update: ", err)
	}
	if cmd.RowsAffected() != 1 {
		log.Println("fx-sdk: submit order: expected 1 row affected, got: ", cmd.RowsAffected())
	}

	return &SubmitOrderResult{
		RefID:    refId,
		OrderDay: orderDay,
		Status:   orderStatus,
		Cause:    resp.GetCause(),
	}, nil
}

// CancelOrder sends a cancellation request to the Core for an existing order.
func (c *Client) CancelOrder(ctx context.Context, p *CancelOrderParams) (*CancelOrderResult, error) {
	if p == nil || p.ClientID == "" {
		return nil, ErrClientIDRequired
	}
	if p.OrderID == 0 {
		return nil, fmt.Errorf("fx-sdk: cancel order: order_id is required")
	}
	if p.OrderDay == "" {
		return nil, fmt.Errorf("fx-sdk: cancel order: order_day is required")
	}
	req := &forexv1.CancelOrderRequest{
		ClientId: &p.ClientID,
		OrderId:  &p.OrderID,
		OrderDay: &p.OrderDay,
	}

	resp, err := c.order.CancelOrder(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: cancel order: %w", err)
	}

	// Update the local order table using the ref_id returned by the Core —
	// that's the local client_orders.ref_id we sent on submit.
	orderStatus := OrderStatus(resp.GetOrderStatus())

	if resp.GetSuccess() && resp.GetRefId() != 0 {
		_, _ = c.db.Exec(ctx,
			`UPDATE client_orders SET status = $1, cause = $2, remaining_quantity = $3, updated_at = NOW()
			  WHERE order_day = $4 AND ref_id = $5`,
			orderStatus, resp.GetCause(), resp.GetRemainingQuantity(), p.OrderDay, resp.GetRefId())
	}

	return &CancelOrderResult{
		Success:           resp.GetSuccess(),
		RemainingQuantity: resp.GetRemainingQuantity(),
		Status:            orderStatus,
		Cause:             resp.GetCause(),
		RefID:             resp.GetRefId(),
	}, nil
}

// FilterClientOrders queries the Core for a client's orders matching the given
// filters. ClientID is mandatory. If OrderDayFrom or OrderDayTo is empty, the
// SDK defaults them to today and today+1 (YYYY-MM-DD) respectively.
func (c *Client) FilterClientOrders(ctx context.Context, p *FilterClientOrdersParams) (*FilterClientOrdersResult, error) {
	if p == nil || p.ClientID == "" {
		return nil, ErrClientIDRequired
	}

	from := p.OrderDayFrom
	to := p.OrderDayTo
	if from == "" || to == "" {
		today := time.Now()
		if from == "" {
			from = today.Format("2006-01-02")
		}
		if to == "" {
			to = today.AddDate(0, 0, 1).Format("2006-01-02")
		}
	}

	req := &forexv1.FilterClientOrdersRequest{
		ClientId:     &p.ClientID,
		OrderDayFrom: &from,
		OrderDayTo:   &to,
	}
	if p.Side != 0 {
		req.Side = new(int32(p.Side))
	}
	if p.Status != 0 {
		req.Status = new(int32(p.Status))
	}
	if p.CurrencyPair != "" {
		req.CurrencyPair = &p.CurrencyPair
	}
	if p.Limit > 0 {
		req.Limit = &p.Limit
	}
	if p.Offset > 0 {
		req.Offset = &p.Offset
	}

	resp, err := c.order.FilterClientOrders(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: filter client orders: %w", err)
	}

	out := &FilterClientOrdersResult{
		Orders: make([]Order, 0, len(resp.GetOrders())),
	}

	for _, o := range resp.GetOrders() {
		out.Orders = append(out.Orders, Order{
			OrderID:           o.GetOrderId(),
			Side:              Side(o.GetSide()),
			Segment:           Segment(o.GetSegment()),
			Status:            OrderStatus(o.GetStatus()),
			AllowPartialFill:  o.GetAllowPartialFill(),
			CurrencyPair:      o.GetCurrencyPair(),
			Quantity:          o.GetQuantity(),
			LimitRate:         o.GetLimitRate(),
			RefID:             o.GetRefId(),
			MinTradeQuantity:  o.GetMinTradeQuantity(),
			RemainingQuantity: o.GetRemainingQuantity(),
			CreatedAt:         o.GetCreatedAt(),
			OrderDay:          o.GetOrderDay(),
		})
	}
	return out, nil
}

// SubscribeOrderEvents subscribes to order status events from the Core.
// When an event is received, it updates the local DB and calls the provided handler.
//
// The stream is reopened on transient gRPC errors (Unavailable,
// ResourceExhausted) and clean server-side EOFs, backing off between attempts.
// The consecutive-failure counter resets on every successful Recv, so a
// subscription that is making progress survives arbitrarily many reconnects.
// The method returns only when ctx is cancelled, the error is not retryable,
// or maxRetries consecutive reconnect attempts have failed.
func (c *Client) SubscribeOrderEvents(ctx context.Context, handler OrderEventHandler) error {
	attempt := 0
	for {
		err := c.runOrderEventStream(ctx, handler, &attempt)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isStreamRetryable(err) {
			return fmt.Errorf("fx-sdk: subscribe order events: %w", err)
		}
		if attempt >= c.maxRetries {
			return fmt.Errorf("fx-sdk: subscribe order events after %d reconnects: %w", attempt, err)
		}
		delay := backoff(attempt, c.baseDelay, c.maxDelay)
		attempt++
		log.Printf("fx-sdk: order event stream disconnected, reconnecting in %s (attempt %d): %v", delay, attempt, err)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// runOrderEventStream opens a single SubscribeOrderEvents stream and pumps
// events until the stream returns an error. On every successful Recv the
// caller's attempt counter is reset so transient flaps do not accumulate
// toward the reconnect cap.
func (c *Client) runOrderEventStream(ctx context.Context, handler OrderEventHandler, attempt *int) error {
	stream, err := c.order.SubscribeOrderEvents(ctx, &forexv1.SubscribeOrderEventsRequest{})
	if err != nil {
		return err
	}
	for {
		resp, err := stream.Recv()
		if err != nil {
			return err
		}
		*attempt = 0

		event := &OrderEvent{
			EventType:         OrderStatus(resp.GetEventType()),
			EventTimestamp:    resp.GetEventTimestamp(),
			RemainingQuantity: resp.GetRemainingQuantity(),
			RefID:             resp.GetRefId(),
		}

		// Order events don't carry order_day, so we fall back to ref_id-only
		// lookup. The ref_id column is still unique (BIGSERIAL) across chunks;
		// TimescaleDB just cannot prune partitions for this path.
		if resp.GetRefId() > 0 {
			cmd, err := c.db.Exec(ctx,
				`UPDATE client_orders SET status = $1, remaining_quantity = $2, updated_at = NOW() WHERE order_day = $3 AND ref_id = $4`,
				event.EventType, event.RemainingQuantity, resp.GetOrderDay(), resp.GetRefId())
			if err != nil {
				log.Println("fx-sdk: update order status: ", err, " refID: ", resp.GetRefId(), " remainingQuantity: ", event.RemainingQuantity)
			} else if cmd.RowsAffected() == 0 {
				log.Println("fx-sdk: no rows affected: ", resp.GetRefId())
			}
		}

		if handler != nil {
			handler(event)
		}
	}
}

// GetOrderBookDepth returns the aggregated order book (bids and asks) for a given currency pair.
func (c *Client) GetOrderBookDepth(ctx context.Context, p *GetOrderBookDepthParams) (*GetOrderBookDepthResult, error) {
	if p.ClientID == "" {
		return nil, fmt.Errorf("fx-sdk: get order book depth: client_id is required")
	}
	if p.Segment < Retail || p.Segment > Treasury {
		return nil, fmt.Errorf("fx-sdk: get order book depth: segment is required")
	}
	req := &forexv1.GetOrderBookDepthRequest{
		Segment:      new(int32(p.Segment)),
		MaxLevels:    &p.MaxLevels,
		ClientId:     &p.ClientID,
		CurrencyPair: &p.CurrencyPair,
	}

	resp, err := c.order.GetOrderBookDepth(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: get order book depth: %w", err)
	}

	out := &GetOrderBookDepthResult{
		Bids: make([]PriceLevel, 0, len(resp.GetBids())),
		Asks: make([]PriceLevel, 0, len(resp.GetAsks())),
	}

	for _, b := range resp.GetBids() {
		out.Bids = append(out.Bids, PriceLevel{
			Rate:          b.GetRate(),
			TotalQuantity: b.GetTotalQuantity(),
		})
	}

	for _, a := range resp.GetAsks() {
		out.Asks = append(out.Asks, PriceLevel{
			Rate:          a.GetRate(),
			TotalQuantity: a.GetTotalQuantity(),
		})
	}

	return out, nil
}
