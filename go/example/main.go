// Example demonstrates how to use the fx-sdk v1 client to submit, cancel
// and query orders against the FX Core via gRPC.
//
// Usage:
//
//	go run ./example \
//	    -target=fx-core.example.com:443 \
//	    -partner-id=YOUR_PARTNER_ID \
//	    -api-key=YOUR_API_KEY \
//	    -dsn="postgres://user:pass@localhost:5432/fx?sslmode=disable" \
//	    -client-id=CLIENT_42 \
//	    -client-inn=123456789 \
//	    -market
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"os"
	"os/signal"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/alifcapital/fx-sdk/go/v1"
	"google.golang.org/grpc"
)

func main() {
	var (
		target    = flag.String("target", "dev-fx-api.alif.tj:443", "FX Core gRPC address")
		sdkId     = flag.String("sdk-id", "01a056da-7ba2-79af-bd2e-20be91f8d24e", "SDK identifier")
		apiKey    = flag.String("api-key", "R81H9BQ+usXmrpXd/rOt9q7aQEQieF14NzX9qsYwY66SrDQmCAtqqbLURvgpkvB5OYKzjP5dF1Qn6CjCtBTrAA==", "API key")
		dsn       = flag.String("dsn", "postgres://postgres:pass123@192.168.215.2:5432/fxdb?sslmode=disable", "Postgres DSN for the local orders table")
		partnerId = flag.String("partner-id", "019fcb0f-33f1-756c-9688-12a0fc8289ea", "partner identifier")
		clientId  = flag.String("client-id", "1275", "client identifier (required for filter)")
		clientINN = flag.String("client-inn", "07128325", "client INN (taxpayer ID)")
		insecureC = flag.Bool("insecure", false, "use plaintext gRPC (dev only)")
		cancel    = flag.Bool("cancel", false, "cancel the order after submission")
		market    = flag.Bool("market", false, "also submit a market order (priced by the book)")
	)
	flag.Parse()

	if *sdkId == "" || *apiKey == "" || *dsn == "" || *clientId == "" || *clientINN == "" {
		log.Fatal("sdk-id, api-key, dsn, client-id and client-inn are required")
	}

	g, ctx := errgroup.WithContext(context.Background())
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// Local Postgres pool used by the SDK to track submitted orders.
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	// Build SDK options. For production, pass real TLS credentials instead of
	// grpc.WithTransportCredentials(insecure.NewCredentials()).
	opts := []v1.Option{
		v1.WithMaxRetries(3),
		v1.WithRetryBackoff(100*time.Millisecond, 5*time.Second),
	}

	if *insecureC {
		opts = append(opts, v1.WithDialOptions(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		))
	} else {
		opts = append(opts, v1.WithDialOptions(
			grpc.WithTransportCredentials(
				credentials.NewTLS(&tls.Config{}),
			),
		))
	}

	client, err := v1.New(*target, *sdkId, *apiKey, *partnerId, pool, opts...)
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	// 0. Subscribe to order events in the background.
	// This will receive real-time updates for any order status changes.
	g.Go(func() error {
		log.Printf("starting order events subscription...")
		err := client.SubscribeOrderEvents(ctx, *partnerId, handleOrderEvent)
		if err != nil && ctx.Err() == nil {
			log.Printf("subscription error: %v", err)
		}
		return err
	})

	// 0b. Subscribe to trade events. The SDK persists the trade, computes
	// settlement and fee, and updates the order; the handler below is
	// responsible for the partner-specific account movements.
	g.Go(func() error {
		log.Printf("starting trade subscription...")
		err := client.SubscribeTrades(ctx, handleTrade)
		if err != nil && ctx.Err() == nil {
			log.Printf("trade subscription error: %v", err)
		}
		return err
	})

	// 0c. Drain any trades that were stored but not settled (handler failure,
	// or a crash after ack but before settlement) on startup, then keep
	// retrying them on a ticker. handleTrade must be idempotent.
	g.Go(func() error {
		if err := client.RetryUnsettled(ctx, handleTrade); err != nil {
			log.Printf("retry unsettled (startup): %v", err)
		}
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				if err := client.RetryUnsettled(ctx, handleTrade); err != nil {
					log.Printf("retry unsettled: %v", err)
				}
			}
		}
	})
	// 0c-bis. Reconcile the hour that just closed against the Core, a few
	// minutes past the hour so the window is settled on both sides. On a
	// mismatch the SDK pulls the Core's trades for that same window, stores
	// anything missing locally and runs handleTrade for it, then re-checks.
	// Whatever is still mismatched after that needs a human — ReconcileDiff
	// says which trades diverged.
	g.Go(func() error {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				// PreviousHour is the last completed hour in the SDK
				// timezone (UTC+05:00). Do not derive it from the host
				// clock: the Core compares the hour in +05, so a host in
				// another zone would ask about the wrong window.
				day, hour := v1.PreviousHour()
				params := &v1.ReconcileParams{
					PartnerId: *partnerId,
					Day:       day,
					Hour:      hour,
				}
				res, err := client.ReconcileRepair(ctx, params, handleTrade)
				if err != nil {
					log.Printf("reconcile repair %s h%02d: %v", params.Day, params.Hour, err)
					continue
				}
				if res.Resolved() {
					log.Printf("reconciled %s h%02d: recovered=%d settled=%d",
						params.Day, params.Hour, res.Recovered, res.Settled)
					continue
				}
				log.Printf("reconcile %s h%02d STILL MISMATCHED: core=%d recovered=%d settled=%d settle_failed=%d failed=%d",
					params.Day, params.Hour, res.CoreTrades, res.Recovered,
					res.Settled, res.SettleFailed, res.Failed)
				diff, derr := client.ReconcileDiff(ctx, params)
				if derr != nil {
					log.Printf("reconcile diff %s h%02d: %v", params.Day, params.Hour, derr)
					continue
				}
				log.Printf("  missing_locally=%d missing_remotely=%d mismatched=%d unsettled=%d",
					len(diff.MissingLocally), len(diff.MissingRemotely),
					len(diff.Mismatched), len(diff.Unsettled))
			}
		}
	})

	// 0d. Fetch the available currency pairs and their trading specs. The
	// partner is identified by metadata, so no parameters are required.
	pairs, err := client.GetCurrencyPairs(ctx)
	if err != nil {
		log.Printf("get currency pairs error: %v", err)
	} else {
		log.Printf("available currency pairs (%d):", len(pairs))
		for _, p := range pairs {
			log.Printf("  %s active=%v min_lot=%s min_trade_qty=%s valid_rate_percent=%d nbt_rate=%s",
				p.Pair, p.IsActive, p.MinLot, p.MinTradeQuantity, p.ValidRatePercent, p.NbtRate)
		}
	}

	segment := v1.Retail
	var acc = make(map[string]string)
	// account details
	acc["debit_account"] = "1274"
	acc["credit_account"] = "4574"
	var fee = make(map[string]string)
	// fixed fee in percentage
	fee["fixed"] = "0.05"

	// 1. Submit a small USD/TJS buy limit order at 9.31. OrderType is left unset,
	// which the SDK treats as v1.LimitOrder.
	submitted, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
		Side:             v1.Sell,
		Segment:          segment,
		AllowPartialFill: true,
		PartnerId:        *partnerId,
		ClientId:         *clientId,
		ClientINN:        *clientINN,
		CurrencyPair:     "USD/TJS",
		Quantity:         "2000.00",
		LimitRate:        "9.280",
		MinTradeQuantity: "100.00",
		Account:          acc,
		Fee:              fee,
	})
	switch {
	case err == v1.ErrDuplicateOrder:
		log.Printf("submit: duplicate detected within 2 minutes, skipping")
	case err != nil:
		log.Fatalf("submit order: %v", err)
	default:
		log.Printf("submitted: ref_id=%d order_day=%s status=%d cause=%q",
			submitted.RefId, submitted.OrderDay, submitted.Status, submitted.Cause)
	}

	// 1b. Submit a market order. It carries no LimitRate — the book prices it,
	// and whatever cannot be filled right away is cancelled rather than left
	// resting. Because the price is not known in advance, the response reports
	// the executed quantity and the weighted average rate synchronously; the
	// individual trades still arrive on the trade subscription.
	if *market {
		mkt, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
			Side:             v1.Sell,
			Segment:          segment,
			AllowPartialFill: true,
			PartnerId:        *partnerId,
			ClientId:         *clientId,
			ClientINN:        *clientINN,
			CurrencyPair:     "USD/TJS",
			Quantity:         "30000.00",
			OrderType:        v1.MarketOrder,
			Account:          acc,
			Fee:              fee,
		})
		switch {
		case err == v1.ErrDuplicateOrder:
			log.Printf("market submit: duplicate detected within 2 minutes, skipping")
		case err != nil:
			log.Printf("market submit: %v", err)
		default:
			log.Printf("market submitted: ref_id=%d order_day=%s status=%d filled=%s avg_rate=%s cause=%q",
				mkt.RefId, mkt.OrderDay, mkt.Status, mkt.FilledQuantity, mkt.AverageRate, mkt.Cause)
		}
	}

	// 2. Filter the client's orders. ClientID is mandatory; the SDK defaults
	// OrderDayFrom/To to today and today+1 when both are empty.
	filtered, err := client.FilterClientOrders(ctx, &v1.FilterClientOrdersParams{
		PartnerId:    *partnerId,
		ClientId:     *clientId,
		CurrencyPair: "USD/TJS",
		Limit:        50,
	})
	if err != nil {
		log.Fatalf("filter orders: %v", err)
	}
	log.Printf("found %d orders for client %s", len(filtered.Orders), *clientId)
	for _, o := range filtered.Orders {
		log.Printf("  order_id=%d day=%s side=%d status=%d type=%d counterparty=%d qty=%s remaining=%s rate=%s ref=%d",
			o.OrderId, o.OrderDay, o.Side, o.Status, o.OrderType, o.CounterpartySegment,
			o.Quantity, o.RemainingQuantity, o.LimitRate, o.RefId)
	}

	// 3. Get order book depth.
	depth, err := client.GetOrderBookDepth(ctx, &v1.GetOrderBookDepthParams{
		Segment:      segment,
		MaxLevels:    10,
		ClientId:     *clientId,
		CurrencyPair: "USD/TJS",
		PartnerId:    *partnerId,
	})
	if err != nil {
		log.Printf("get order book depth error: %v", err)
	} else {
		log.Printf("Order Book USD/TJS:")
		log.Printf("  Asks:")
		for _, v := range slices.Backward(depth.Asks) {
			log.Printf("    %s: %s", v.Rate, v.TotalQuantity)
		}
		log.Printf("  Bids:")
		for _, b := range depth.Bids {
			log.Printf("    %s: %s", b.Rate, b.TotalQuantity)
		}
	}

	// 4. Cancel the order we just submitted (only if the submit succeeded).
	if *cancel && submitted != nil {
		cancelled, err := client.CancelOrder(ctx, &v1.CancelOrderParams{
			PartnerId: *partnerId,
			ClientId:  *clientId,
			OrderId:   submitted.RefId,
			OrderDay:  submitted.OrderDay,
		})
		if err != nil {
			log.Fatalf("cancel order: %v", err)
		}
		log.Printf("cancelled: success=%v remaining=%s status=%d cause=%q",
			cancelled.Success, cancelled.RemainingQuantity, cancelled.Status, cancelled.Cause)
	}

	// Wait briefly to allow the background subscription to catch any final events.
	if err := g.Wait(); err != nil {
		log.Fatalln("shutdown:", err)
	}
}

func handleTrade(ctx context.Context, ev *v1.TradeEvent) error {
	log.Printf("TRADE: id=%d order=%d side=%d order_status=%d  day=%s filled=%s rate=%s settlement=%s fee=%s side=%d pair=%s",
		ev.TradeId, ev.OrderId, ev.Side, ev.OrderStatus, ev.TradingDay, ev.FilledQuantity, ev.ExecutionRate,
		ev.Settlement, ev.Fee, ev.Side, ev.CurrencyPair)
	log.Printf("  accounts: debit=%s credit=%s -> move %s %s (fee %s)",
		ev.Account["debit_account"], ev.Account["credit_account"], ev.Settlement, ev.CurrencyPair, ev.Fee)
	return nil
}

func handleOrderEvent(event *v1.OrderEvent) {
	log.Printf("EVENT: ref_id=%d type=%d ts=%s remaining=%s",
		event.RefId, event.EventType, event.EventTimestamp, event.RemainingQuantity)
}
