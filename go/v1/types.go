package v1

import (
	"errors"
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
	Account          []byte // optional JSONB
	Fee              []byte // optional JSONB
}

// SubmitOrderResult contains the result of a submitted order.
type SubmitOrderResult struct {
	RefID    string      // local UUID used as ref_id (idempotency key)
	OrderID  int64       // remote order ID assigned by the Core
	OrderDay string      // YYYY-MM-DD derived from the UUIDv7 timestamp
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
	RefID             string      // local ref_id associated with the order
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
	RefID             string
	MinTradeQuantity  string
	RemainingQuantity string
	CreatedAt         string
	OrderDay          string
}

// FilterClientOrdersResult contains the orders matching a filter query.
type FilterClientOrdersResult struct {
	Orders []Order
}
