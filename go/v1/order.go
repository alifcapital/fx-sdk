package v1

import (
	"context"
	"fmt"
	"time"

	"github.com/alifcapital/fx-sdk/go/forexv1"
)

const orderDayLayout = "2006-01-02"

// SubmitOrder inserts the order into the local database, derives ref_id and
// order_day from the generated UUIDv7, then forwards the order to the Core.
// The local row is updated with the Core's response status.
func (c *Client) SubmitOrder(ctx context.Context, p *SubmitOrderParams) (*SubmitOrderResult, error) {
	// 0. Check for duplicates in the last 2 minutes.
	// Duplicate is defined as same side, limit_rate, client_id, and quantity.
	// This prevents the client from accidentally double-submitting the same order request within a short timeframe.
	var exists bool
	err := c.db.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM orders
			WHERE uuid_extract_timestamp(id) > NOW() - INTERVAL '2 minutes'
			  AND side = $1
			  AND limit_rate = $2
			  AND client_id = $3
			  AND quantity = $4
			  AND currency_pair = $5);`, p.Side, p.LimitRate, p.ClientID, p.Quantity, p.CurrencyPair).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: check duplicate: %w", err)
	}
	if exists {
		return nil, ErrDuplicateOrder
	}

	// 1. Insert into the local orders table; the DB generates a UUIDv7 id.
	var id, orderDay string
	err = c.db.QueryRow(ctx,
		`INSERT INTO orders (side, segment, quantity, limit_rate, remaining_quantity,
		                     min_trade_quantity, allow_partial_fill, currency_pair,
		                     client_id, client_inn, account, fee)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING id, uuid_extract_timestamp(id);`,
		p.Side, p.Segment,
		p.Quantity, p.LimitRate, p.Quantity, // remaining_quantity starts equal to quantity
		p.MinTradeQuantity,
		p.AllowPartialFill,
		p.CurrencyPair, p.ClientID, p.ClientINN,
		p.Account, p.Fee,
	).Scan(&id, &orderDay)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: insert order: %w", err)
	}
	orderDay = orderDay[:10]
	// 3. Build and send the gRPC request.
	req := &forexv1.SubmitOrderRequest{
		Side:             new(int32(p.Side)),
		Segment:          new(int32(p.Segment)),
		AllowPartialFill: new(p.AllowPartialFill),
		ClientId:         &p.ClientID,
		ClientInn:        &p.ClientINN,
		CurrencyPair:     &p.CurrencyPair,
		Quantity:         &p.Quantity,
		LimitRate:        &p.LimitRate,
		RefId:            &id,
		OrderDay:         &orderDay,
		MinTradeQuantity: &p.MinTradeQuantity,
	}

	resp, err := c.order.SubmitOrder(ctx, req)
	if err != nil {
		// Mark the local order as Failed so it is not retried blindly.
		_, _ = c.db.Exec(ctx,
			`UPDATE orders SET status = $1, cause = $2, updated_at = NOW() WHERE id = $3`, Failed, err.Error(), id)
		return nil, fmt.Errorf("fx-sdk: submit order: %w", err)
	}

	// 4. Reflect the Core's response into the local row.
	orderStatus := OrderStatus(resp.GetStatus())
	_, _ = c.db.Exec(ctx,
		`UPDATE orders SET status = $1, cause = $2, order_id = $3, updated_at = NOW() WHERE id = $4`,
		orderStatus, resp.GetCause(), resp.GetOrderId(), id)

	return &SubmitOrderResult{
		RefID:    id,
		OrderID:  resp.GetOrderId(),
		OrderDay: orderDay,
		Status:   orderStatus,
		Cause:    resp.GetCause(),
	}, nil
}

// CancelOrder sends a cancellation request to the Core for an existing order.
func (c *Client) CancelOrder(ctx context.Context, p *CancelOrderParams) (*CancelOrderResult, error) {
	req := &forexv1.CancelOrderRequest{
		ClientId: &p.ClientID,
		OrderId:  &p.OrderID,
		OrderDay: &p.OrderDay,
	}

	resp, err := c.order.CancelOrder(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: cancel order: %w", err)
	}

	// Update the local order table using the ref_id or order_id returned by the Core.
	// We use the Core's returned status and cause.
	orderStatus := OrderStatus(resp.GetOrderStatus())
	refID := resp.GetRefId()
	if resp.GetSuccess() && refID != "" {
		_, _ = c.db.Exec(ctx,
			`UPDATE orders SET status = $1, cause = $2, remaining_quantity = $3, updated_at = NOW() WHERE id = $4`,
			orderStatus, resp.GetCause(), resp.GetRemainingQuantity(), refID)
	}

	return &CancelOrderResult{
		Success:           resp.GetSuccess(),
		RemainingQuantity: resp.GetRemainingQuantity(),
		Status:            orderStatus,
		Cause:             resp.GetCause(),
		RefID:             refID,
	}, nil
}

// FilterClientOrders queries the Core for a client's orders matching the given
// filters. ClientID is mandatory. If OrderDayFrom or OrderDayTo is empty, the
// SDK defaults them to today and today+1 (UTC, YYYY-MM-DD) respectively.
func (c *Client) FilterClientOrders(ctx context.Context, p *FilterClientOrdersParams) (*FilterClientOrdersResult, error) {
	if p == nil || p.ClientID == "" {
		return nil, ErrClientIDRequired
	}

	from := p.OrderDayFrom
	to := p.OrderDayTo
	if from == "" || to == "" {
		today := time.Now().UTC()
		if from == "" {
			from = today.Format(orderDayLayout)
		}
		if to == "" {
			to = today.AddDate(0, 0, 1).Format(orderDayLayout)
		}
	}

	req := &forexv1.FilterClientOrdersRequest{
		ClientId:     &p.ClientID,
		OrderDayFrom: &from,
		OrderDayTo:   &to,
	}
	if p.OrderID != 0 {
		req.OrderId = &p.OrderID
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
