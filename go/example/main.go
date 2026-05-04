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
//	    -client-inn=123456789
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/alifcapital/fx-sdk/go/v1"
	"google.golang.org/grpc"
)

func main() {
	var (
		target    = flag.String("target", "192.168.97.7:80", "FX Core gRPC address")
		partnerID = flag.String("partner-id", "019d99bd-24db-7a04-b237-76a89e84375b", "partner identifier")
		apiKey    = flag.String("api-key", "tmSh1BqPuChtU1VSj5fTAip1FR6nueMK2Vdd+JlgiKbpyx5bv5vFBmMtYcT7+qerMYo1ukpFikZXsQ+cS7psDQ==", "API key")
		dsn       = flag.String("dsn", "postgres://postgres:pas123@192.168.215.2:5432/fxdb?sslmode=disable", "Postgres DSN for the local orders table")
		clientID  = flag.String("client-id", "1263", "client identifier (required for filter)")
		clientINN = flag.String("client-inn", "07128313", "client INN (taxpayer ID)")
		insecureC = flag.Bool("insecure", true, "use plaintext gRPC (dev only)")
		cancel    = flag.Bool("cancel", false, "cancel the order after submission")
	)
	flag.Parse()

	if *partnerID == "" || *apiKey == "" || *dsn == "" || *clientID == "" || *clientINN == "" {
		log.Fatal("partner-id, api-key, dsn, client-id and client-inn are required")
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
	}

	client, err := v1.New(*target, *partnerID, *apiKey, pool, opts...)
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	// 0. Subscribe to order events in the background.
	// This will receive real-time updates for any order status changes.
	g.Go(func() error {
		log.Printf("starting order events subscription...")
		err := client.SubscribeOrderEvents(ctx, handleOrderEvent)
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

	var acc = make(map[string]string)
	// account details
	acc["debit_account"] = "1263"
	acc["credit_account"] = "4563"
	var fee = make(map[string]string)
	// fixed fee in percentage
	fee["fixed"] = "0.05"

	// 1. Submit a small USD/TJS buy order at limit 9.60.
	submitted, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
		Side:             v1.Buy,
		Segment:          v1.Retail,
		AllowPartialFill: true,
		ClientID:         *clientID,
		ClientINN:        *clientINN,
		CurrencyPair:     "USD/TJS",
		Quantity:         "1000.00",
		LimitRate:        "9.438",
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
			submitted.RefID, submitted.OrderDay, submitted.Status, submitted.Cause)
	}

	// 2. Filter the client's orders. ClientID is mandatory; the SDK defaults
	// OrderDayFrom/To to today and today+1 when both are empty.
	filtered, err := client.FilterClientOrders(ctx, &v1.FilterClientOrdersParams{
		ClientID:     *clientID,
		CurrencyPair: "USD/TJS",
		Limit:        50,
	})
	if err != nil {
		log.Fatalf("filter orders: %v", err)
	}
	log.Printf("found %d orders for client %s", len(filtered.Orders), *clientID)
	for _, o := range filtered.Orders {
		log.Printf("  order_id=%d day=%s side=%d status=%d qty=%s remaining=%s rate=%s ref=%d",
			o.OrderID, o.OrderDay, o.Side, o.Status,
			o.Quantity, o.RemainingQuantity, o.LimitRate, o.RefID)
	}

	// 4. Get order book depth.
	depth, err := client.GetOrderBookDepth(ctx, &v1.GetOrderBookDepthParams{
		Segment:      v1.Retail,
		MaxLevels:    10,
		ClientID:     *clientID,
		CurrencyPair: "USD/TJS",
	})
	if err != nil {
		log.Printf("get order book depth error: %v", err)
	} else {
		log.Printf("Order Book USD/TJS:")
		log.Printf("  Asks:")
		for i := len(depth.Asks) - 1; i >= 0; i-- {
			log.Printf("    %s: %s", depth.Asks[i].Rate, depth.Asks[i].TotalQuantity)
		}
		log.Printf("  Bids:")
		for _, b := range depth.Bids {
			log.Printf("    %s: %s", b.Rate, b.TotalQuantity)
		}
	}

	// 3. Cancel the order we just submitted (only if the submit succeeded).
	if *cancel && submitted != nil {
		cancelled, err := client.CancelOrder(ctx, &v1.CancelOrderParams{
			ClientID: *clientID,
			OrderID:  123213,
			OrderDay: submitted.OrderDay,
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
	log.Printf("TRADE: id=%d  day=%s filled=%s rate=%s settlement=%s fee=%s side=%d pair=%s",
		ev.TradeID, ev.TradingDay, ev.FilledQuantity, ev.ExecutionRate,
		ev.Settlement, ev.Fee, ev.Side, ev.CurrencyPair)
	log.Printf("  accounts: debit=%s credit=%s -> move %s %s (fee %s)",
		ev.Account["debit_account"], ev.Account["credit_account"], ev.Settlement, ev.CurrencyPair, ev.Fee)
	return nil
}

func handleOrderEvent(event *v1.OrderEvent) {
	log.Printf("EVENT: ref_id=%d type=%d ts=%s remaining=%s",
		event.RefID, event.EventType, event.EventTimestamp, event.RemainingQuantity)
}
