package v1

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/alifcapital/fx-sdk/go/forexv1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
)

const reconPartner = "p1"

// statusCore answers GetOrderStatuses from a fixed table keyed by ref_id, the
// way the Core does: one entry per key, found or not.
type statusCore struct {
	forexv1.OrderServiceClient
	known    map[int64]*forexv1.OrderStatusInfo
	requests []*forexv1.GetOrderStatusesRequest
}

func (s *statusCore) GetOrderStatuses(_ context.Context, req *forexv1.GetOrderStatusesRequest, _ ...grpc.CallOption) (*forexv1.GetOrderStatusesResponse, error) {
	s.requests = append(s.requests, req)
	resp := &forexv1.GetOrderStatusesResponse{}
	for _, k := range req.GetKeys() {
		info := &forexv1.OrderStatusInfo{OrderDay: new(k.GetOrderDay()), RefId: new(k.GetRefId()), Found: new(false)}
		if o, ok := s.known[k.GetRefId()]; ok {
			info.Found = new(true)
			info.OrderId, info.Status, info.RemainingQuantity = o.OrderId, o.Status, o.RemainingQuantity
			for _, t := range o.Trades {
				t.OrderDay = new(k.GetOrderDay())
				info.Trades = append(info.Trades, t)
			}
		}
		resp.Orders = append(resp.Orders, info)
	}
	return resp, nil
}

// testPool connects to FXSDK_TEST_DB and creates client_orders and
// client_trades, exactly as db.sql declares them, in a schema private to this
// test. Without the variable the test is skipped.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("FXSDK_TEST_DB")
	if dsn == "" {
		t.Skip("FXSDK_TEST_DB not set")
	}
	ddl, err := os.ReadFile("../db.sql")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	schema := "sdk_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	for _, table := range []string{"client_orders", "client_trades"} {
		m := regexp.MustCompile(`(?s)CREATE TABLE ` + table + ` \(.*?\n\)[^;]*;`).Find(ddl)
		if m == nil {
			t.Fatalf("%s not found in db.sql", table)
		}
		if _, err := pool.Exec(ctx, string(m)); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
	}
	return pool
}

// seedOrder stores a local order of 100 submitted age ago, with the given
// status and remaining, plus the trades this SDK already holds for it.
func seedOrder(t *testing.T, pool *pgxpool.Pool, refId int64, partner string, st OrderStatus, remaining string, age time.Duration, trades ...*forexv1.TradeResponse) string {
	t.Helper()
	ctx := context.Background()
	var day string
	if err := pool.QueryRow(ctx, `
		INSERT INTO client_orders (submitted_at, ref_id, side, segment, status, quantity, remaining_quantity,
		                           currency_pair, partner_id, client_id, client_inn)
		VALUES (NOW() - $1::interval, $2, 1, 1, $3, 100, $4::numeric, 'USD/TJS', $5, 'c1', '123456789')
		RETURNING order_day::text`, age, refId, int16(st), remaining, partner).Scan(&day); err != nil {
		t.Fatal(err)
	}
	for _, tr := range trades {
		if _, err := pool.Exec(ctx, `
			INSERT INTO client_trades (trading_day, trade_id, order_id, ref_id, side, filled_quantity,
			                           execution_rate, partner_id, client_id, settled)
			VALUES ($1, $2, $3, $4, 1, $5::numeric, 9.27, $6, 'c1', TRUE)`,
			day, tr.GetId(), tr.GetOrderId(), refId, tr.GetFilledQuantity(), partner); err != nil {
			t.Fatal(err)
		}
	}
	return day
}

// trade builds a Core trade of the order refId (order id refId*10) on today's
// trading day; its order day is filled in by statusCore.
func trade(id, refId int64, qty string, orderStatus OrderStatus) *forexv1.TradeResponse {
	return &forexv1.TradeResponse{
		Id: new(id), RefId: new(refId), OrderId: new(refId * 10), OrderStatus: new(int32(orderStatus)),
		Side: new(int32(Buy)), TradingDay: new(Today()), FilledQuantity: new(qty), ExecutionRate: new("9.27"),
		ExecutedAt: new(time.Now().Add(-26 * time.Hour).Format(time.RFC3339)), PartnerId: new(reconPartner),
		TradeType: new(int32(3)),
	}
}

func core(refId int64, st OrderStatus, remaining string, trades ...*forexv1.TradeResponse) *forexv1.OrderStatusInfo {
	return &forexv1.OrderStatusInfo{
		OrderId: new(refId * 10), Status: new(int32(st)), RemainingQuantity: new(remaining), Trades: trades,
	}
}

type orderState struct {
	status    OrderStatus
	remaining string
	orderId   *int64
	cause     *string
}

func readOrder(t *testing.T, pool *pgxpool.Pool, refId int64) orderState {
	t.Helper()
	var (
		o  orderState
		st int16
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT status, remaining_quantity::text, order_id, cause FROM client_orders WHERE ref_id = $1`, refId,
	).Scan(&st, &o.remaining, &o.orderId, &o.cause); err != nil {
		t.Fatal(err)
	}
	o.status = OrderStatus(st)
	return o
}

func countTrades(t *testing.T, pool *pgxpool.Pool, refId int64) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM client_trades WHERE ref_id = $1`, refId).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every case of the order reconciliation, each on its own order.
func TestReconcileOrders(t *testing.T) {
	pool := testPool(t)
	const old = 30 * time.Hour

	// 1–3: nothing traded, the event never arrived.
	seedOrder(t, pool, 1, reconPartner, Pending, "100", old)
	seedOrder(t, pool, 2, reconPartner, Pending, "100", old)
	seedOrder(t, pool, 3, reconPartner, Pending, "100", old)
	// 4: never reached the Core.
	seedOrder(t, pool, 4, reconPartner, Pending, "100", old)
	// 5: filled by two trades this SDK never got.
	seedOrder(t, pool, 5, reconPartner, Pending, "100", old)
	// 6: one trade and the expiry both lost.
	seedOrder(t, pool, 6, reconPartner, Pending, "100", old)
	// 7: one trade here, the last one lost.
	seedOrder(t, pool, 7, reconPartner, FilledPartially, "60", old, trade(71, 7, "40", FilledPartially))
	// 8: both trades here, delivered out of order, so the status went back.
	seedOrder(t, pool, 8, reconPartner, FilledPartially, "0", old,
		trade(81, 8, "40", FilledPartially), trade(82, 8, "60", Filled))
	// 9: one trade here, one trade and the expiry lost.
	seedOrder(t, pool, 9, reconPartner, FilledPartially, "80", old, trade(91, 9, "20", FilledPartially))
	// 10A: the trade arrived, the expiry did not.
	seedOrder(t, pool, 10, reconPartner, FilledPartially, "80", old, trade(101, 10, "20", FilledPartially))
	// 10B: the expiry arrived (80 released), then the trade came late and
	// took 20 off a second time.
	seedOrder(t, pool, 11, reconPartner, FilledPartially, "60", old, trade(111, 11, "20", FilledPartially))
	// 11: the Core has not expired it yet.
	seedOrder(t, pool, 12, reconPartner, Pending, "100", old)
	// The Core reports a fill whose trade it has not returned yet.
	seedOrder(t, pool, 13, reconPartner, Pending, "100", old)
	// Not selected: too young, another partner, already closed here.
	seedOrder(t, pool, 14, reconPartner, Pending, "100", time.Hour)
	seedOrder(t, pool, 15, "p2", Pending, "100", old)
	seedOrder(t, pool, 16, reconPartner, Expired, "100", old)
	// Closed by its order event (remainder cancelled, 70 left) while its one
	// trade of 30 never arrived: the trade is stored, nothing else changes.
	seedOrder(t, pool, 17, reconPartner, CancelledPartially, "70", old)
	// Closed and complete: not looked at.
	seedOrder(t, pool, 18, reconPartner, Filled, "0", old, trade(181, 18, "100", Filled))

	c := &Client{db: pool, order: &statusCore{known: map[int64]*forexv1.OrderStatusInfo{
		1:  core(1, Expired, "100.000000"),
		2:  core(2, Cancelled, "100.000000"),
		3:  core(3, Failed, "100.000000"),
		5:  core(5, Filled, "0.000000", trade(51, 5, "60", FilledPartially), trade(52, 5, "40", Filled)),
		6:  core(6, ExpiredPartially, "70.000000", trade(61, 6, "30", FilledPartially)),
		7:  core(7, Filled, "0.000000", trade(71, 7, "40", FilledPartially), trade(72, 7, "60", Filled)),
		8:  core(8, Filled, "0.000000", trade(81, 8, "40", FilledPartially), trade(82, 8, "60", Filled)),
		9:  core(9, ExpiredPartially, "50.000000", trade(91, 9, "20", FilledPartially), trade(92, 9, "30", FilledPartially)),
		10: core(10, ExpiredPartially, "80.000000", trade(101, 10, "20", FilledPartially)),
		11: core(11, ExpiredPartially, "80.000000", trade(111, 11, "20", FilledPartially)),
		12: core(12, Pending, "100.000000"),
		13: core(13, Filled, "0.000000", trade(131, 13, "40", FilledPartially)),
		14: core(14, Expired, "100.000000"),
		15: core(15, Expired, "100.000000"),
		16: core(16, Expired, "100.000000"),
		17: core(17, CancelledPartially, "70.000000", trade(171, 17, "30", CancelledPartially)),
		18: core(18, Filled, "0.000000", trade(181, 18, "100", Filled)),
	}}}

	var settledTrades []int64
	releases := map[int64]*OrderEvent{}
	res, err := c.ReconcileOrders(context.Background(), &ReconcileOrdersParams{PartnerId: reconPartner},
		func(_ context.Context, e *TradeEvent) error {
			settledTrades = append(settledTrades, e.TradeId)
			return nil
		},
		func(e *OrderEvent) { releases[e.RefId] = e })
	if err != nil {
		t.Fatal(err)
	}

	want := ReconcileOrdersResult{
		Checked:     14, // 1–13 and 17
		TradesAdded: 7,  // 51, 52, 61, 72, 92, 131, 171; nothing for 8, 10, 11, which had theirs
		Completed:   1,  // 17
		Closed:      10, // 1, 2, 3, 5, 6, 7, 8, 9, 10, 11
		Released:    7,  // 1, 2, 3, 4, 6, 9, 10
		Corrected:   1,  // 11
		Lost:        1,  // 4
		StillLive:   1,  // 12
		Behind:      1,  // 13
	}
	if *res != want {
		t.Errorf("result = %+v\n          want %+v", *res, want)
	}

	for _, tc := range []struct {
		ref       int64
		status    OrderStatus
		remaining string
		release   string // "" = no release
	}{
		{1, Expired, "100.000000", "100.000000"},
		{2, Cancelled, "100.000000", "100.000000"},
		{3, Failed, "100.000000", "100.000000"},
		{4, Failed, "100.000000", "100.000000"},
		{5, Filled, "0.000000", ""},
		{6, ExpiredPartially, "70.000000", "70.000000"},
		{7, Filled, "0.000000", ""},
		{8, Filled, "0.000000", ""},
		{9, ExpiredPartially, "50.000000", "50.000000"},
		{10, ExpiredPartially, "80.000000", "80.000000"},
		{11, ExpiredPartially, "80.000000", ""}, // released already when its event arrived
		{12, Pending, "100.000000", ""},
		{13, FilledPartially, "60.000000", ""}, // its trade stored, the order waits for the rest
		{14, Pending, "100.000000", ""},
		{15, Pending, "100.000000", ""},
		{16, Expired, "100.000000", ""},
		{17, CancelledPartially, "70.000000", ""}, // its remainder was released when it closed
		{18, Filled, "0.000000", ""},
	} {
		got := readOrder(t, pool, tc.ref)
		if got.status != tc.status || got.remaining != tc.remaining {
			t.Errorf("order %d = %v %s, want %v %s", tc.ref, got.status, got.remaining, tc.status, tc.remaining)
		}
		ev, released := releases[tc.ref]
		switch {
		case tc.release == "" && released:
			t.Errorf("order %d released %s, want no release", tc.ref, ev.RemainingQuantity)
		case tc.release != "" && !released:
			t.Errorf("order %d not released, want %s", tc.ref, tc.release)
		case released && (ev.RemainingQuantity != tc.release || ev.EventType != tc.status || ev.OrderDay == ""):
			t.Errorf("order %d release = %+v, want %v %s", tc.ref, *ev, tc.status, tc.release)
		}
		if amt, ok := ev.getRelease(); released && (!ok || amt != tc.release) {
			t.Errorf("order %d: GetReleaseAmount = %q, %v", tc.ref, amt, ok)
		}
	}

	if o := readOrder(t, pool, 4); o.cause == nil || *o.cause != CauseNotInCore {
		t.Errorf("order 4 cause = %v, want %q", o.cause, CauseNotInCore)
	}
	if o := readOrder(t, pool, 1); o.orderId == nil || *o.orderId != 10 {
		t.Errorf("order 1 order_id = %v, want 10 from the Core", o.orderId)
	}
	for ref, n := range map[int64]int{5: 2, 6: 1, 7: 2, 8: 2, 9: 2, 13: 1, 17: 1} {
		if got := countTrades(t, pool, ref); got != n {
			t.Errorf("order %d holds %d trades, want %d", ref, got, n)
		}
	}
	if len(settledTrades) != 7 {
		t.Errorf("trade handler ran for %v, want the 7 added trades", settledTrades)
	}

	// A second run releases nothing again and stores no trade twice; only the
	// two orders still waiting are looked at.
	again, err := c.ReconcileOrders(context.Background(), &ReconcileOrdersParams{PartnerId: reconPartner}, nil,
		func(e *OrderEvent) { t.Errorf("second run released order %d", e.RefId) })
	if err != nil || again.Checked != 2 || again.TradesAdded != 0 || again.Released != 0 {
		t.Errorf("second run = %+v, %v", again, err)
	}
}

// getRelease is GetReleaseAmount on a possibly nil event.
func (e *OrderEvent) getRelease() (string, bool) {
	if e == nil {
		return "", false
	}
	return e.GetReleaseAmount()
}

func TestReconcileOrdersBatches(t *testing.T) {
	pool := testPool(t)
	known := map[int64]*forexv1.OrderStatusInfo{}
	for i := int64(1); i <= orderStatusBatch+20; i++ {
		seedOrder(t, pool, i, reconPartner, Pending, "100", 30*time.Hour)
		known[i] = core(i, Pending, "100.000000")
	}
	sc := &statusCore{known: known}
	c := &Client{db: pool, order: sc}

	res, err := c.ReconcileOrders(context.Background(), &ReconcileOrdersParams{PartnerId: reconPartner}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Checked != orderStatusBatch+20 || len(sc.requests) != 2 {
		t.Fatalf("checked %d in %d requests, want %d in 2", res.Checked, len(sc.requests), orderStatusBatch+20)
	}
	if n := len(sc.requests[0].GetKeys()); n != orderStatusBatch {
		t.Errorf("first request carried %d keys, want %d", n, orderStatusBatch)
	}
}

// A trade never closes an order and never touches one already closed: the
// order event that cancels a market order's remainder arrives before its
// trades, and trades can arrive out of order.
func TestPersistTradeLeavesClosedOrderAlone(t *testing.T) {
	pool := testPool(t)
	c := &Client{db: pool}
	ctx := context.Background()
	store := func(tr *forexv1.TradeResponse, day string) {
		t.Helper()
		tr.OrderDay = new(day)
		if _, _, _, err := c.persistTrade(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}

	// Market order of 100, trades of 30 and 40 whose snapshot status is
	// already CancelledPartially: both count, and the order stays open for its
	// event.
	day := seedOrder(t, pool, 1, reconPartner, Pending, "100", time.Minute)
	store(trade(11, 1, "30", CancelledPartially), day)
	store(trade(12, 1, "40", CancelledPartially), day)
	if o := readOrder(t, pool, 1); o.status != FilledPartially || o.remaining != "30.000000" {
		t.Errorf("trades first: %v %s, want FilledPartially 30", o.status, o.remaining)
	}

	// The event came first (CancelledPartially, 30 left); its trades follow.
	day = seedOrder(t, pool, 2, reconPartner, CancelledPartially, "30", time.Minute)
	store(trade(21, 2, "30", CancelledPartially), day)
	store(trade(22, 2, "40", CancelledPartially), day)
	if o := readOrder(t, pool, 2); o.status != CancelledPartially || o.remaining != "30.000000" {
		t.Errorf("event first: %v %s, want CancelledPartially 30 untouched", o.status, o.remaining)
	}

	// The last trade arrives first and fills the order; the earlier one then
	// neither decrements nor puts the status back.
	day = seedOrder(t, pool, 3, reconPartner, Pending, "100", time.Minute)
	store(trade(32, 3, "60", Filled), day)
	store(trade(31, 3, "40", FilledPartially), day)
	if o := readOrder(t, pool, 3); o.status != Filled || o.remaining != "0.000000" {
		t.Errorf("out of order: %v %s, want Filled 0", o.status, o.remaining)
	}

	// Every trade is stored regardless.
	for ref, n := range map[int64]int{1: 2, 2: 2, 3: 2} {
		if got := countTrades(t, pool, ref); got != n {
			t.Errorf("order %d holds %d trades, want %d", ref, got, n)
		}
	}
}
