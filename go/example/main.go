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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/credentials/insecure"

	v1 "github.com/alifcapital/fx-sdk/go/v1"
	"google.golang.org/grpc"
)

func main() {
	var (
		target    = flag.String("target", "localhost:9090", "FX Core gRPC address")
		partnerID = flag.String("partner-id", "", "partner identifier")
		apiKey    = flag.String("api-key", "", "API key")
		dsn       = flag.String("dsn", "", "Postgres DSN for the local orders table")
		clientID  = flag.String("client-id", "", "client identifier (required for filter)")
		clientINN = flag.String("client-inn", "", "client INN (taxpayer ID)")
		insecureC = flag.Bool("insecure", true, "use plaintext gRPC (dev only)")
	)
	flag.Parse()

	if *partnerID == "" || *apiKey == "" || *dsn == "" || *clientID == "" || *clientINN == "" {
		log.Fatal("partner-id, api-key, dsn, client-id and client-inn are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

	// 1. Submit a small EUR/USD buy order at limit 1.0850.
	submitted, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
		Side:             v1.Buy,
		Segment:          v1.Retail,
		AllowPartialFill: true,
		ClientID:         *clientID,
		ClientINN:        *clientINN,
		CurrencyPair:     "EUR/USD",
		Quantity:         "1000.00",
		LimitRate:        "1.0850",
		MinTradeQuantity: "100.00",
	})
	switch {
	case err == v1.ErrDuplicateOrder:
		log.Printf("submit: duplicate detected within 2 minutes, skipping")
	case err != nil:
		log.Fatalf("submit order: %v", err)
	default:
		log.Printf("submitted: ref_id=%s order_id=%d status=%d cause=%q",
			submitted.RefID, submitted.OrderID, submitted.Status, submitted.Cause)
	}

	// 2. Filter the client's orders. ClientID is mandatory; the SDK defaults
	// OrderDayFrom/To to today and today+1 when both are empty.
	filtered, err := client.FilterClientOrders(ctx, &v1.FilterClientOrdersParams{
		ClientID:     *clientID,
		CurrencyPair: "EUR/USD",
		Limit:        50,
	})
	if err != nil {
		log.Fatalf("filter orders: %v", err)
	}
	log.Printf("found %d orders for client %s", len(filtered.Orders), *clientID)
	for _, o := range filtered.Orders {
		log.Printf("  order_id=%d day=%s side=%d status=%d qty=%s remaining=%s rate=%s ref=%s",
			o.OrderID, o.OrderDay, o.Side, o.Status,
			o.Quantity, o.RemainingQuantity, o.LimitRate, o.RefID)
	}

	// 3. Cancel the order we just submitted (only if the submit succeeded).
	if submitted != nil && submitted.OrderID != 0 {
		cancelled, err := client.CancelOrder(ctx, &v1.CancelOrderParams{
			ClientID: *clientID,
			OrderID:  submitted.OrderID,
			OrderDay: submitted.OrderDay,
		})
		if err != nil {
			log.Fatalf("cancel order: %v", err)
		}
		log.Printf("cancelled: success=%v remaining=%s status=%d cause=%q",
			cancelled.Success, cancelled.RemainingQuantity, cancelled.Status, cancelled.Cause)
	}
}
