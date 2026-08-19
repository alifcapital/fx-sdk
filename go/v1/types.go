package v1

import (
	"context"
	"errors"
	"fmt"

	"github.com/alifcapital/fx-sdk/go/pkg"
	"github.com/govalues/decimal"
)

// OrderStatus represents the lifecycle state of an order.
type OrderStatus int8

const (
	Unknown            OrderStatus = 0
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
	// ErrPartnerIDRequired is returned when partner_id is not provided.
	ErrPartnerIDRequired = errors.New("fx-sdk: partner_id is required")
	// ErrLimitRateRequired is returned when a limit order carries no limit rate.
	ErrLimitRateRequired = errors.New("fx-sdk: limit_rate is required for a limit order")
	// ErrInvalidOrderType is returned when order_type is neither limit nor market.
	ErrInvalidOrderType = errors.New("fx-sdk: order_type must be LimitOrder or MarketOrder")
	// ErrInvalidCounterpartySegment is returned when counterparty_segment is set
	// to anything other than AnyCounterparty or Treasury.
	ErrInvalidCounterpartySegment = errors.New("fx-sdk: counterparty_segment must be AnyCounterparty or Treasury")
	// ErrTreasuryCounterpartyOnly is returned when a non-treasury order asks to
	// trade against treasury counterparties only.
	ErrTreasuryCounterpartyOnly = errors.New("fx-sdk: only a Treasury order may set CounterpartySegment = Treasury")
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
	// AnyCounterparty is the zero Segment. It is not a valid order segment; it
	// is the default for SubmitOrderParams.CounterpartySegment and means the
	// order may match a counterparty in any segment.
	AnyCounterparty Segment = 0
	Retail          Segment = 1
	Corporate       Segment = 2
	Treasury        Segment = 3
)

// OrderType selects how the order is priced and what happens to the part of it
// that cannot be filled immediately.
type OrderType int32

const (
	// LimitOrder executes at LimitRate or better. Whatever is not filled
	// immediately rests in the order book until it fills, expires or is
	// cancelled. This is the default when OrderType is left unset.
	LimitOrder OrderType = 1
	// MarketOrder executes against the best available prices in the book and
	// ignores LimitRate. Its unfilled remainder is cancelled instead of resting
	// in the book, so a market order never leaves a live order behind.
	//
	// Because the partner cannot know the price in advance, the fill of a market
	// order is reported synchronously in SubmitOrderResult.FilledQuantity and
	// AverageRate, in addition to arriving on the trade stream.
	MarketOrder OrderType = 2
)

// SubmitOrderParams contains the parameters for submitting a new order.
// The SDK derives ref_id and order_day automatically from the local DB insert.
type SubmitOrderParams struct {
	Side             Side
	Segment          Segment
	AllowPartialFill bool
	PartnerId        string
	ClientId         string
	ClientINN        string
	CurrencyPair     string
	Quantity         string // decimal string, e.g. "1000.00"
	LimitRate        string // decimal string, e.g. "10.8500"; required for LimitOrder, ignored for MarketOrder
	MinTradeQuantity string // optional; relevant when AllowPartialFill is true
	Account          any    // optional JSONB
	Fee              any    // optional JSONB
	// OrderType selects limit or market execution. Zero means LimitOrder.
	OrderType OrderType
	// CounterpartySegment restricts which counterparties the order may match.
	// Zero (AnyCounterparty) places no restriction; only a Treasury order may
	// set Treasury to trade against treasury counterparties exclusively.
	CounterpartySegment Segment
}

// SubmitOrderResult contains the result of a submitted order.
type SubmitOrderResult struct {
	RefId    int64       // local client_orders.id used as ref_id (idempotency key); stringified on the wire
	OrderDay string      // YYYY-MM-DD; value of the local client_orders.order_day column
	Status   OrderStatus // status returned by the Core
	Cause    string      // reason if the order was rejected/failed
	// FilledQuantity and AverageRate report the synchronous execution of a
	// MarketOrder: the base-currency quantity actually executed and the
	// quantity-weighted average rate across the trades of this submission.
	// Both are empty for a LimitOrder, whose fills arrive on the trade stream.
	//
	// The individual trades behind these totals also arrive on the trade
	// stream, which stays the authoritative source for settlement — these
	// fields exist so the caller learns the market price without waiting.
	FilledQuantity string
	AverageRate    string
}

// CancelOrderParams contains the parameters for cancelling an existing order.
type CancelOrderParams struct {
	PartnerId string
	ClientId  string
	OrderId   int64
	OrderDay  string
}

// CancelOrderResult contains the result of a cancelled order.
type CancelOrderResult struct {
	Success           bool
	RemainingQuantity string      // remaining qty at cancellation, for fund release
	Status            OrderStatus // updated order status
	Cause             string      // reason if cancellation failed
	RefId             int64       // local client_orders.id associated with the order
}

// FilterClientOrdersParams contains the parameters for querying a client's orders.
// ClientID is mandatory. If OrderDayFrom or OrderDayTo is empty, the SDK defaults
// them to today and today+1 (YYYY-MM-DD) respectively.
type FilterClientOrdersParams struct {
	PartnerId    string // required
	ClientId     string // required
	RefId        int64  // optional; 0 means unset
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
	OrderId           int64
	Side              Side
	Segment           Segment
	Status            OrderStatus
	AllowPartialFill  bool
	CurrencyPair      string
	Quantity          string
	LimitRate         string
	RefId             int64 // local client_orders.id; 0 if the Core returned an empty or unparseable ref_id
	MinTradeQuantity  string
	RemainingQuantity string
	CreatedAt         string
	OrderDay          string
	OrderType         OrderType // LimitOrder or MarketOrder
	// CounterpartySegment is AnyCounterparty when the order may match any
	// segment, or Treasury when it is restricted to treasury counterparties.
	CounterpartySegment Segment
}

// OrderEvent represents an event received from the Core via SubscribeOrderEvents.
type OrderEvent struct {
	RefId             int64 // local client_orders.id
	EventType         OrderStatus
	EventTimestamp    string
	RemainingQuantity string
	PartnerId         string
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
	ClientId     string
	CurrencyPair string
	PartnerId    string
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

// CurrencyPair describes a tradable currency pair and its trading specifications.
type CurrencyPair struct {
	Pair             string // e.g. "USD/TJS"
	MinLot           string // minimum lot size (decimal string)
	MinTradeQuantity string // minimum tradable quantity (decimal string)
	ValidRatePercent int32  // % band around the rate considered valid
	NbtRate          string // NBT rate (decimal string)
	IsActive         bool
}

// TradeEvent represents a single trade execution received from the Core, enriched
// with data from the parent order (client, side, currency pair, account, fee
// config) and with the settlement/fee values already computed and persisted.
type TradeEvent struct {
	TradeId        int64
	OrderId        int64 // core order id; part of the client_trades primary key
	OrderStatus    OrderStatus
	TradingDay     string // YYYY-MM-DD
	FilledQuantity string // base-currency amount filled by this trade
	ExecutionRate  string // rate at which the fill occurred
	ExecutedAt     string // server-side execution timestamp
	PartnerId      string
	ClientId       string
	CurrencyPair   string
	Side           Side
	Account        map[string]string // account JSONB stored on the parent order
	FeeConfig      map[string]string // fee JSONB stored on the parent order
	Settlement     decimal.Decimal   // (filled_quantity * execution_rate)+-Fee
	Fee            decimal.Decimal   // settlement * fee_config.fixed / 100
}

func (e *TradeEvent) Cal() error {
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
	e.Fee, err = pkg.Percentage(m, e.Fee)
	if err != nil {
		return fmt.Errorf("compute fee: %w", err)
	}
	// side: buy / sell
	if e.Side == Buy {
		e.Settlement, err = m.Add(e.Fee)
		if err != nil {
			return fmt.Errorf("compute settlement: %w", err)
		}
		e.Settlement = e.Settlement.Round(6)
		return nil
	}
	e.Settlement, err = m.Sub(e.Fee)
	if err != nil {
		return fmt.Errorf("compute settlement: %w", err)
	}
	e.Settlement = e.Settlement.Round(6)
	return nil
}

// TradeEventHandler performs the partner-side processing for a trade — typically
// debit/credit of the client's accounts based on TradeEvent.Account and the
// computed Settlement/Fee.
//
// The trade has already been durably stored before the handler runs, so a
// returned error does NOT change what the SDK acks to the Core (the ack only
// means "received & stored"). Instead the trade is left settled = FALSE with the
// error recorded, and RetryUnsettled re-runs the handler until it succeeds.
//
// IMPORTANT: the handler MUST be idempotent. The same trade
// (trade_id, order_id, trading_day) may be presented more than once — on Core
// redelivery after a reconnect, or on retry after a failure or restart. The
// partner's account movement must no-op if it has already been applied for that
// trade.
type TradeEventHandler func(ctx context.Context, event *TradeEvent) error
