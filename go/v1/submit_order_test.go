package v1

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/alifcapital/fx-sdk/go/forexv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// submitCore answers SubmitOrder with a fixed response or error.
type submitCore struct {
	forexv1.OrderServiceClient
	resp  *forexv1.SubmitOrderResponse
	err   error
	calls int
}

func (s *submitCore) SubmitOrder(context.Context, *forexv1.SubmitOrderRequest, ...grpc.CallOption) (*forexv1.SubmitOrderResponse, error) {
	s.calls++
	return s.resp, s.err
}

func submitParams(qty string) *SubmitOrderParams {
	return &SubmitOrderParams{
		Side: Buy, Segment: Retail, PartnerId: reconPartner, ClientId: "c1", ClientINN: "123456789",
		CurrencyPair: "USD/TJS", Quantity: qty, LimitRate: "10.95",
	}
}

func rowCount(t *testing.T, c *Client) int {
	t.Helper()
	var n int
	if err := c.db.QueryRow(context.Background(), `SELECT count(*) FROM client_orders`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every SubmitOrder outcome the README describes: what is returned, and what
// the local row looks like afterwards.
func TestSubmitOrderOutcomes(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	resp := func(st OrderStatus, cause string, orderId int64) *forexv1.SubmitOrderResponse {
		return &forexv1.SubmitOrderResponse{Status: new(int32(st)), Cause: new(cause), OrderId: new(orderId)}
	}

	for i, tc := range []struct {
		name       string
		core       *submitCore
		wantErr    error // nil: no error
		wantResult bool
		wantStatus OrderStatus // local row; Unknown = no row written
		wantOrder  bool        // order_id recorded
	}{
		{"accepted", &submitCore{resp: resp(Pending, "", 42)}, nil, true, Pending, true},
		{"rejected by the Core", &submitCore{resp: resp(Rejected, "курс вне допустимого коридора", 0)}, nil, true, Rejected, false},
		{"failed in the Core", &submitCore{resp: resp(Failed, "не удалось добавить ордер в стакан", 43)}, nil, true, Failed, true},
		{"duplicate in the Core", &submitCore{resp: resp(Duplicate, "дубликат ордера", 0)}, nil, true, Pending, false},
		{"refused: unauthenticated", &submitCore{err: status.Error(codes.Unauthenticated, "неверный api-key")}, ErrOrderNotAccepted, false, Rejected, false},
		{"refused: invalid argument", &submitCore{err: status.Error(codes.InvalidArgument, "bad")}, ErrOrderNotAccepted, false, Rejected, false},
		{"unknown: unavailable", &submitCore{err: status.Error(codes.Unavailable, "connection refused")}, ErrOrderOutcomeUnknown, true, Pending, false},
		{"unknown: deadline", &submitCore{err: status.Error(codes.DeadlineExceeded, "deadline")}, ErrOrderOutcomeUnknown, true, Pending, false},
		{"unknown: internal", &submitCore{err: status.Error(codes.Internal, "boom")}, ErrOrderOutcomeUnknown, true, Pending, false},
		{"unknown: context", &submitCore{err: context.DeadlineExceeded}, ErrOrderOutcomeUnknown, true, Pending, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{db: pool, order: tc.core}
			// A distinct quantity per case keeps the SDK's one-minute duplicate check out of the way.
			res, err := c.SubmitOrder(ctx, submitParams(strconv.Itoa((i+1)*100)))
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("err = %v, want none", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if (res != nil) != tc.wantResult {
				t.Fatalf("result = %+v, want present: %v", res, tc.wantResult)
			}
			if res == nil {
				// Only the refusals return no result; find their row by its status.
				var st int16
				var cause *string
				if err := pool.QueryRow(ctx, `SELECT status, cause FROM client_orders ORDER BY ref_id DESC LIMIT 1`).Scan(&st, &cause); err != nil {
					t.Fatal(err)
				}
				if OrderStatus(st) != tc.wantStatus || cause == nil || *cause == "" {
					t.Fatalf("row = %d cause %v, want %v with the Core's reason", st, cause, tc.wantStatus)
				}
				return
			}
			var st int16
			var orderId *int64
			if err := pool.QueryRow(ctx, `SELECT status, order_id FROM client_orders WHERE ref_id = $1 AND order_day = $2`,
				res.RefId, res.OrderDay).Scan(&st, &orderId); err != nil {
				t.Fatal(err)
			}
			if OrderStatus(st) != tc.wantStatus || (orderId != nil) != tc.wantOrder {
				t.Errorf("row = %d order_id %v, want %v with order id: %v", st, orderId, tc.wantStatus, tc.wantOrder)
			}
			// The rule an application follows.
			if errors.Is(err, ErrOrderOutcomeUnknown) && res.RefId == 0 {
				t.Error("an unknown outcome must still tell the caller its ref_id")
			}
		})
	}
}

// Errors found before anything is written or sent: no row, no call.
func TestSubmitOrderRefusedLocally(t *testing.T) {
	pool := testPool(t)
	core := &submitCore{resp: &forexv1.SubmitOrderResponse{Status: new(int32(Pending))}}
	c := &Client{db: pool, order: core}
	ctx := context.Background()

	bad := submitParams("100")
	bad.Segment = 0
	if _, err := c.SubmitOrder(ctx, bad); !errors.Is(err, ErrInvalidSegment) {
		t.Errorf("segment: err = %v, want ErrInvalidSegment", err)
	}
	if _, err := c.SubmitOrder(ctx, submitParams("1.1234567")); !errors.Is(err, ErrDecimalPrecision) {
		t.Errorf("precision: err = %v, want ErrDecimalPrecision", err)
	}
	if n := rowCount(t, c); n != 0 || core.calls != 0 {
		t.Fatalf("rows = %d, calls = %d, want nothing written or sent", n, core.calls)
	}

	// The same order again within a minute: the SDK does not send it.
	if _, err := c.SubmitOrder(ctx, submitParams("100")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitOrder(ctx, submitParams("100")); !errors.Is(err, ErrDuplicateOrder) {
		t.Errorf("repeat: err = %v, want ErrDuplicateOrder", err)
	}
	if n := rowCount(t, c); n != 1 || core.calls != 1 {
		t.Errorf("rows = %d, calls = %d, want only the first order", n, core.calls)
	}
}
