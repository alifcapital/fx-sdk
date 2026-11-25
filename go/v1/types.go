package v1

import (
	"context"
	"errors"
	"fmt"

	"github.com/alifcapital/fx-sdk/go/pkg"
	"github.com/quagmt/udecimal"
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
	// ErrDuplicateOrder is returned when the same order was submitted moments ago.
	ErrDuplicateOrder = errors.New("fx-sdk: такой же ордер уже был отправлен только что, повторите позже")
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
	// ErrInvalidDay is returned when a day parameter is not a YYYY-MM-DD date.
	ErrInvalidDay = errors.New("fx-sdk: day must be a YYYY-MM-DD date")
	// ErrInvalidSegment is returned when segment is not Retail, Corporate or Treasury.
	ErrInvalidSegment = errors.New("fx-sdk: segment must be Retail, Corporate or Treasury")

	// ErrOrderNotAccepted wraps a SubmitOrder failure after which no order
	// exists in the Core: the local database failed before the request was
	// sent, or the Core refused the request outright (authentication,
	// permissions, malformed request) before storing anything. Funds reserved
	// for this call can be released. If the local row was already written it
	// is marked Rejected with the reason, so nothing later releases it again.
	ErrOrderNotAccepted = errors.New("fx-sdk: ордер не принят")
	// ErrOrderOutcomeUnknown wraps a SubmitOrder failure after which the order
	// may or may not exist in the Core: the request was sent and no usable
	// answer came back (network failure, timeout, cancelled context, an error
	// the SDK cannot classify). Keep the reserved funds and do not submit again
	// — that could create a second order. If the order exists, its trades and
	// events arrive on the streams; if it does not, ReconcileOrders marks it
	// Failed and releases it through the order handler. SubmitOrder returns a
	// non-nil result with RefId and OrderDay alongside this error, so the
	// caller can tie what arrives later to this submission.
	ErrOrderOutcomeUnknown = errors.New("fx-sdk: неизвестно, принят ли ордер")
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
	// Its fills arrive on the trade stream and the cancelled remainder as a
	// Cancelled or CancelledPartially order event, like any other outcome; the
	// SubmitOrder response only says the order was accepted.
	MarketOrder OrderType = 2
)

// TradeType says who stood on the two sides of a trade, as reported by the Core.
// The zero value means the Core did not report it (a trade stored before the
// field existed, or an older Core).
type TradeType int32

const (
	// SamePartner: one partner on both sides, no treasury (Retail ↔ Retail).
	SamePartner TradeType = 1
	// SamePartnerTreasury: one partner on both sides, one of them Treasury.
	SamePartnerTreasury TradeType = 2
	// CrossPartner: different partners on the two sides, any segments.
	CrossPartner TradeType = 3
)

// SubmitOrderParams contains the parameters for submitting a new order.
// The SDK derives ref_id and order_day automatically from the local DB insert.
type SubmitOrderParams struct {
	Side         Side
	Segment      Segment
	PartnerId    string
	ClientId     string
	ClientINN    string
	CurrencyPair string
	Quantity     string // decimal string, e.g. "1000.00"
	LimitRate    string // decimal string, e.g. "10.8500"; required for LimitOrder, ignored for MarketOrder
	// MinTradeQuantity is the smallest quantity one trade may take from a limit
	// order (decimal string). Empty means the currency pair's MinTradeQuantity;
	// a smaller value is rejected by the Core. Ignored for MarketOrder.
	MinTradeQuantity string
	Account          any // optional JSONB
	Fee              any // optional JSONB
	// OrderType selects limit or market execution. Zero means LimitOrder.
	OrderType OrderType
	// CounterpartySegment restricts which counterparties the order may match.
	// Zero (AnyCounterparty) places no restriction; only a Treasury order may
	// set Treasury to trade against treasury counterparties exclusively.
	CounterpartySegment Segment
}

// SubmitOrderResult contains the result of a submitted order.
type SubmitOrderResult struct {
	RefId    int64  // local client_orders.id used as ref_id (idempotency key); stringified on the wire
	OrderId  int64  // Core order id, the one CancelOrder takes; 0 if rejected before the Core saved it
	OrderDay string // YYYY-MM-DD; value of the local client_orders.order_day column
	// Status says only whether the Core accepted the order: Pending (accepted —
	// what matching does to it arrives on the trade and order event streams),
	// Rejected, Duplicate or Failed.
	Status OrderStatus
	Cause  string // reason if the order was rejected/failed
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
	CurrencyPair      string
	Quantity          string
	LimitRate         string
	RefId             int64 // local client_orders.id; 0 if the Core returned an empty or unparseable ref_id
	MinTradeQuantity  string
	RemainingQuantity string
	CreatedAt         string
	OrderDay          string
	ExpiresAt         string    // RFC3339; empty if the Core did not report one
	OrderType         OrderType // LimitOrder or MarketOrder
	// CounterpartySegment is AnyCounterparty when the order may match any
	// segment, or Treasury when it is restricted to treasury counterparties.
	CounterpartySegment Segment
}

// OrderEvent represents an event received from the Core via SubscribeOrderEvents.
type OrderEvent struct {
	RefId             int64  // local client_orders.ref_id
	OrderDay          string // local client_orders.order_day, 'YYYY-MM-DD'
	EventType         OrderStatus
	EventTimestamp    string
	RemainingQuantity string
	PartnerId         string
}

// GetReleaseAmount returns the remaining quantity if the event is an expiration
// or cancellation, indicating that funds should be released. Failed comes only
// from ReconcileOrders, for an order that never reached the Core.
func (e *OrderEvent) GetReleaseAmount() (string, bool) {
	switch e.EventType {
	case Expired, ExpiredPartially, Cancelled, CancelledPartially, Failed:
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
	TickSize         string // price step: LimitRate must be a multiple of it (decimal string)
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
	TradeType      TradeType
	Account        map[string]string // account JSONB stored on the parent order
	FeeConfig      map[string]string // fee JSONB stored on the parent order
	Settlement     udecimal.Decimal  // (filled_quantity * execution_rate)+-Fee
	Fee            udecimal.Decimal  // settlement * fee_config.fixed / 100
}

func (e *TradeEvent) Cal() error {
	qty, err := udecimal.Parse(e.FilledQuantity)
	if err != nil {
		return fmt.Errorf("parse filled_quantity: %w", err)
	}
	rate, err := udecimal.Parse(e.ExecutionRate)
	if err != nil {
		return fmt.Errorf("parse execution_rate: %w", err)
	}
	// Mul, Add and Sub cannot overflow: udecimal falls back to big.Int, so only
	// parsing and the division inside Percentage can fail here.
	m := qty.Mul(rate)
	// fixed fee in percentage %0.5
	fx, ok := e.FeeConfig["fixed"]
	if !ok {
		fx = "0"
	}
	e.Fee, err = udecimal.Parse(fx)
	if err != nil {
		return fmt.Errorf("parse fee.fixed: %w", err)
	}
	e.Fee, err = pkg.Percentage(m, e.Fee)
	if err != nil {
		return fmt.Errorf("compute fee: %w", err)
	}
	// side: buy / sell
	if e.Side == Buy {
		e.Settlement = m.Add(e.Fee).RoundBank(6)
		return nil
	}
	e.Settlement = m.Sub(e.Fee).RoundBank(6)
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

// GetTradesParams selects the window of Core-side trades to fetch: one day,
// optionally narrowed to an explicit time range.
type GetTradesParams struct {
	PartnerId string // required
	Day       string // required; YYYY-MM-DD
	DtFrom    string // optional; "YYYY-MM-DD HH:MM:SS", defaults to Day 00:00:00
	DtTo      string // optional; "YYYY-MM-DD HH:MM:SS", defaults to the start of the next day
}

// Trade is the SDK-level representation of a trade as the Core recorded it,
// with no local enrichment. It is what GetTrades returns — the counterpart to
// TradeEvent, which is the same trade after the SDK has joined it to the parent
// order and computed settlement and fee.
type Trade struct {
	TradeId        int64
	OrderId        int64
	RefId          int64 // local client_orders.ref_id the trade belongs to
	OrderStatus    OrderStatus
	Side           Side
	OrderDay       string // YYYY-MM-DD
	TradingDay     string // YYYY-MM-DD
	FilledQuantity string
	ExecutionRate  string
	ExecutedAt     string
	PartnerId      string
	TradeType      TradeType
}
