package v1

import (
	"context"
	"errors"
	"fmt"

	"github.com/govalues/decimal"
)

// OrderStatus represents the lifecycle state of an order.
type OrderStatus int8

const (
	Pending            OrderStatus = 1
	FilledPartially    OrderStatus = 2
	Filled             OrderStatus = 3
	Cancelled          OrderStatus = 4
	Expired            OrderStatus = 5
	Failed             OrderStatus = 6
	CancelledPartially OrderStatus = 7
	ExpiredPartially   OrderStatus = 8
	Rejected           OrderStatus = 9
	Duplicate          OrderStatus = 10
	Unknown            OrderStatus = 11
)

var (
	// ErrDuplicateOrder is returned when a duplicate order is detected within 2 minutes.
	ErrDuplicateOrder = errors.New("fx-sdk: duplicate order detected within 2 minutes")
	// ErrClientIDRequired is returned when client_id is not provided.
	ErrClientIDRequired = errors.New("fx-sdk: client_id is required")
)

// Side represents the order direction.
type Side int32

const (
	Buy  Side = 1
	Sell Side = 2
)

// Segment represents the market segment.
type Segment int32

const (
	Retail    Segment = 1
	Corporate Segment = 2
	Treasury  Segment = 3
)

// SubmitOrderParams contains the parameters for submitting a new order.
// The SDK derives ref_id and order_day automatically from the local DB insert.
type SubmitOrderParams struct {
	Side             Side
	Segment          Segment
	AllowPartialFill bool
	ClientID         string
	ClientINN        string
	CurrencyPair     string
	Quantity         string // decimal string, e.g. "1000.00"
	LimitRate        string // decimal string, e.g. "10.8500"
	MinTradeQuantity string // optional; relevant when AllowPartialFill is true
	Account          any    // optional JSONB
	Fee              any    // optional JSONB
}

// SubmitOrderResult contains the result of a submitted order.
type SubmitOrderResult struct {
	RefID    int64       // local client_orders.id used as ref_id (idempotency key); stringified on the wire
	OrderID  int64       // remote order ID assigned by the Core
	OrderDay string      // YYYY-MM-DD; value of the local client_orders.order_day column
	Status   OrderStatus // status returned by the Core
	Cause    string      // reason if the order was rejected/failed
}

// CancelOrderParams contains the parameters for cancelling an existing order.
type CancelOrderParams struct {
	ClientID string
	OrderID  int64
	OrderDay string
}

// CancelOrderResult contains the result of a cancelled order.
type CancelOrderResult struct {
	Success           bool
	RemainingQuantity string      // remaining qty at cancellation, for fund release
	Status            OrderStatus // updated order status
	Cause             string      // reason if cancellation failed
	RefID             int64       // local client_orders.id associated with the order
}

// FilterClientOrdersParams contains the parameters for querying a client's orders.
// ClientID is mandatory. If OrderDayFrom or OrderDayTo is empty, the SDK defaults
// them to today and today+1 (YYYY-MM-DD) respectively.
type FilterClientOrdersParams struct {
	ClientID     string // required
	OrderID      int64  // optional; 0 means unset
	Side         Side   // optional; 0 means unset
	Status       OrderStatus
	CurrencyPair string
	OrderDayFrom string // YYYY-MM-DD; defaults to today
	OrderDayTo   string // YYYY-MM-DD; defaults to today+1
	Limit        int32  // max 100
	Offset       int32
}

// Order is the SDK-level representation of a single order returned by the Core.
type Order struct {
	OrderID           int64
	Side              Side
	Segment           Segment
	Status            OrderStatus
	AllowPartialFill  bool
	CurrencyPair      string
	Quantity          string
	LimitRate         string
	RefID             int64 // local client_orders.id; 0 if the Core returned an empty or unparseable ref_id
	MinTradeQuantity  string
	RemainingQuantity string
	CreatedAt         string
	OrderDay          string
}

// OrderEvent represents an event received from the Core via SubscribeOrderEvents.
type OrderEvent struct {
	EventType         OrderStatus
	EventTimestamp    string
	RemainingQuantity string
	RefID             int64 // local client_orders.id
}

// GetReleaseAmount returns the remaining quantity if the event is an expiration or cancellation,
// indicating that funds should be released.
func (e *OrderEvent) GetReleaseAmount() (string, bool) {
	switch e.EventType {
	case Expired, ExpiredPartially, Cancelled, CancelledPartially:
		return e.RemainingQuantity, true
	default:
		return "", false
	}
}

// OrderEventHandler is a function that handles order events.
type OrderEventHandler func(event *OrderEvent)

// FilterClientOrdersResult contains the orders matching a filter query.
type FilterClientOrdersResult struct {
	Orders []Order
}

// GetOrderBookDepthParams contains the parameters for querying the order book depth.
type GetOrderBookDepthParams struct {
	Segment      Segment
	MaxLevels    int32
	ClientID     string
	CurrencyPair string
}

// PriceLevel represents a single price level in the order book.
type PriceLevel struct {
	Rate          string
	TotalQuantity string
}

// GetOrderBookDepthResult contains the bids and asks for the order book.
type GetOrderBookDepthResult struct {
	Bids []PriceLevel
	Asks []PriceLevel
}

// TradeEvent represents a single trade execution received from the Core, enriched
// with data from the parent order (client, side, currency pair, account, fee
// config) and with the settlement/fee values already computed and persisted.
type TradeEvent struct {
	TradeID        int64
	OrderStatus    OrderStatus
	TradingDay     string // YYYY-MM-DD
	FilledQuantity string // base-currency amount filled by this trade
	ExecutionRate  string // rate at which the fill occurred
	ExecutedAt     string // server-side execution timestamp
	ClientID       string
	CurrencyPair   string
	Side           Side
	Account        map[string]string // account JSONB stored on the parent order
	FeeConfig      map[string]string // fee JSONB stored on the parent order
	Settlement     decimal.Decimal   // (filled_quantity * execution_rate)+-Fee
	Fee            decimal.Decimal   // settlement * fee_config.fixed / 100
}

func (e *TradeEvent) Cal() error {
	sad, _ := decimal.New(100, 0)
	qty, err := decimal.Parse(e.FilledQuantity)
	if err != nil {
		return fmt.Errorf("parse filled_quantity: %w", err)
	}
	rate, err := decimal.Parse(e.ExecutionRate)
	if err != nil {
		return fmt.Errorf("parse execution_rate: %w", err)
	}
	m, err := qty.Mul(rate)
	if err != nil {
		return fmt.Errorf("compute settlement: %w", err)
	}
	// fixed fee in percentage %0.5
	fx, ok := e.FeeConfig["fixed"]
	if !ok {
		fx = "0"
	}
	e.Fee, err = decimal.Parse(fx)
	if err != nil {
		return fmt.Errorf("parse fee.fixed: %w", err)
	}
	e.Fee, err = m.Mul(e.Fee)
	if err != nil {
		return fmt.Errorf("compute fee: %w", err)
	}
	e.Fee, err = e.Fee.Quo(sad)
	if err != nil {
		return fmt.Errorf("compute fee: %w", err)
	}
	// side: buy / sell
	if e.Side == Buy {
		e.Settlement, err = m.Add(e.Fee)
		if err != nil {
			return fmt.Errorf("compute settlement: %w", err)
		}
		return nil
	}
	e.Settlement, err = m.Sub(e.Fee)
	if err != nil {
		return fmt.Errorf("compute settlement: %w", err)
	}
	return nil
}

// TradeEventHandler performs the partner-side processing for a trade — typically
// debit/credit of the client's accounts based on TradeEvent.Account and the
// computed Settlement/Fee. Returning a non-nil error causes the SDK to ack the
// trade back to the Core as "failed" and mark the local trades row accordingly.
type TradeEventHandler func(ctx context.Context, event *TradeEvent) error
