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
	// ErrInvalidDay is returned when a day parameter is not a YYYY-MM-DD date.
	ErrInvalidDay = errors.New("fx-sdk: day must be a YYYY-MM-DD date")
	// ErrInvalidHour is returned when an hour parameter is outside 0..23.
	ErrInvalidHour = errors.New("fx-sdk: hour must be in 0..23")
	// ErrInvalidRecoverRange is returned when the recovery window is inverted
	// or wider than maxRecoverDays.
	ErrInvalidRecoverRange = errors.New("fx-sdk: invalid recovery day range")
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

// ReconcileParams selects the one-hour window to reconcile against the Core.
// The window is [Day Hour:00:00, Day Hour+1:00:00) on client_trades.executed_at.
type ReconcileParams struct {
	PartnerId string // required
	Day       string // required; YYYY-MM-DD, matched against client_trades.trading_day
	Hour      int    // required; 0..23, the hour of Day to reconcile
}

// ReconcileResult is the outcome of reconciling one hour, as persisted in the
// reconciliations table.
type ReconcileResult struct {
	Day  string
	Hour int
	// LocalHash and RemoteHash are the checksums that were actually compared:
	// the tuple checksum against a Core that reports one, the legacy id-only
	// checksum against an older Core. HashVersion says which.
	LocalHash  int64
	RemoteHash int64
	// HashVersion is 2 for the tuple checksum and 1 when the Core answered
	// without one. Version 1 cannot see a lost row whose trade_id equals its
	// order_id, and cannot see a quantity or rate that differs between the two
	// sides at all, so a match reported under it is a weaker statement.
	HashVersion  int
	LocalTrades  int64 // trades that fed LocalHash
	RemoteTrades int64 // the Core's count for the same window; -1 if it sent none
	// Matched reports whether the two checksums agree. A false value is not an
	// error — it means this hour diverged and needs a drill-down.
	Matched bool
	// Done is the stored is_done flag: true once the hour needs no further
	// attention, either because it matched or because a partner closed it by
	// hand after investigating a mismatch. It never reverts to false.
	Done bool
}

// ReconcileInfo is the JSON payload written to reconciliations.info. It records
// what the two sides actually reported, so a mismatch can be investigated after
// the fact without re-running the check.
type ReconcileInfo struct {
	LocalHash    int64  `json:"local_hash"`
	RemoteHash   int64  `json:"remote_hash"`
	HashVersion  int    `json:"hash_version"`
	LocalTrades  int64  `json:"local_trades"`
	RemoteTrades int64  `json:"remote_trades"`
	DtFrom       string `json:"dt_from"`
	DtTo         string `json:"dt_to"`
	CheckedAt    string `json:"checked_at"` // RFC3339, UTC
	// The legacy checksums are kept alongside so an hour checked during the
	// rollout can be compared against one checked before it.
	LocalHashV1  int64 `json:"local_hash_v1"`
	RemoteHashV1 int64 `json:"remote_hash_v1"`
}

// GetTradesParams selects the window of Core-side trades to fetch. It is the
// drill-down counterpart to ReconcileParams: same day, but an explicit time
// range rather than an hour slot, so a whole day can be pulled in one call.
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
}

// LocalTrade is a client_trades row as stored by the partner, including the
// bookkeeping columns the Core knows nothing about (ack, settlement state).
// Those columns are the point: they usually explain why an hour diverged.
type LocalTrade struct {
	TradeId        int64
	OrderId        int64
	RefId          int64
	Side           Side
	FilledQuantity string
	ExecutionRate  string
	ExecutedAt     string
	Ack            bool   // the trade was acked to the Core (received & stored)
	Settled        bool   // partner-side settlement completed; does not affect the checksum
	SettleAttempts int16  // settlement handler attempts so far
	SettleError    string // last settlement error, empty once settled
}

// TradeMismatch is a trade both sides have under the same (trade_id, order_id)
// but do not agree on. Fields names the columns that differ, so a caller can
// log the disagreement without diffing the two structs itself.
type TradeMismatch struct {
	TradeId int64
	OrderId int64
	Fields  []string // e.g. ["filled_quantity", "execution_rate"]
	Core    Trade
	Local   LocalTrade
}

// TradeDiff explains a divergent reconciliation hour trade by trade. Every
// slice is empty when the two sides agree; Unsettled may be non-empty even
// then, since it reports local settlement state rather than a disagreement
// with the Core.
type TradeDiff struct {
	Day         string
	Hour        int
	CoreTrades  int // trades the Core returned for the window
	LocalTrades int // client_trades rows in the window
	// MissingLocally are trades the Core has that never reached the local
	// database — the case that actually loses money, since no settlement ran.
	MissingLocally []Trade
	// MissingRemotely are local trades the Core did not return. Usually a
	// window or timezone artefact rather than a real divergence, since the SDK
	// only ever stores trades the Core pushed.
	MissingRemotely []LocalTrade
	// Mismatched are trades present on both sides with differing values.
	Mismatched []TradeMismatch
	// Unsettled are local trades still awaiting partner-side settlement. They
	// are not a divergence — both sides hold them and the data agrees — and do
	// not affect the checksum; they are reported because an hour under
	// investigation is a good moment to see that money has not moved.
	// RetryUnsettled is what clears them.
	Unsettled []LocalTrade
}

// Divergent reports whether the hour holds a genuine disagreement with the Core
// — a trade missing on one side, or one both sides hold with different values.
// Unsettled trades alone are not a divergence: the data matches, the partner
// has simply not finished settling it.
func (d *TradeDiff) Divergent() bool {
	return len(d.MissingLocally) > 0 || len(d.MissingRemotely) > 0 || len(d.Mismatched) > 0
}

// RepairResult reports what one ReconcileRepair run found and did.
type RepairResult struct {
	Day  string
	Hour int
	// Before is the reconciliation that decided whether a repair was needed. A
	// Before.Matched of true means the hour was already clean and none of the
	// counters below ran.
	Before *ReconcileResult
	// After is the re-check taken once the repair ran, and is the hour's
	// current state whenever it is non-nil. It is nil both when no repair was
	// needed and when the repair changed nothing — in either case Before is
	// still current. An After.Matched of false is an hour that needs a human:
	// run ReconcileDiff on it.
	After *ReconcileResult
	// CoreTrades is how many trades the Core returned for the window; it is how
	// much was examined, not how much was wrong.
	CoreTrades int
	// Recovered is how many of those were missing locally and were stored by
	// this run. It is the number that matters — anything non-zero is trade data
	// the stream never delivered.
	Recovered int
	// Settled counts trades whose settlement handler this run ran successfully,
	// whether this run stored them or an earlier one did.
	Settled int
	// SettleFailed counts trades stored but whose handler returned an error.
	// They are left settled = FALSE with the error recorded, exactly as on the
	// stream, so RetryUnsettled picks them up.
	SettleFailed int
	// Failed counts trades that could not be stored at all, e.g. their parent
	// order is missing from client_orders. These are logged and skipped; a
	// non-zero value needs investigation.
	Failed int
}

// Resolved reports whether the hour is clean now — it either reconciled on the
// first check or matched again after the repair. A false value means the hour
// still diverges in a way ReconcileRepair cannot fix, and ReconcileDiff is the
// next step.
func (r *RepairResult) Resolved() bool {
	if r.Before != nil && r.Before.Matched {
		return true
	}
	return r.After != nil && r.After.Matched
}

// RecoverParams selects the days to catch up on after downtime. DayTo is
// inclusive and defaults to DayFrom.
type RecoverParams struct {
	PartnerId string // required
	DayFrom   string // required; YYYY-MM-DD
	DayTo     string // optional; YYYY-MM-DD, inclusive, defaults to DayFrom
}

// RecoverResult reports what a RecoverTrades run found and did. CoreTrades is
// how much was examined; Recovered is how much was actually missing, and is the
// number that matters — a healthy catch-up after a short outage recovers a
// handful, a zero means the stream had already delivered everything.
type RecoverResult struct {
	Days       []string // the days pulled, oldest first
	CoreTrades int      // trades the Core returned across the window
	Recovered  int      // of those, trades this run stored because they were missing locally
	Settled    int      // trades the handler settled during this run
	// SettleFailed counts trades stored but whose settlement handler returned
	// an error. They are left settled = FALSE with the error recorded, exactly
	// as on the stream, so RetryUnsettled picks them up.
	SettleFailed int
	// Failed counts trades that could not be stored at all, e.g. their parent
	// order is missing from client_orders. These are logged and skipped; a
	// non-zero value needs investigation.
	Failed int
}
