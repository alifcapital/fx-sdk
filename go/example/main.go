// Example демонстрирует, как использовать клиент fx-sdk v1 для подачи, отмены
// и запроса ордеров в FX Core через gRPC.
//
// Использование:
//
//	go run ./example \
//	    -target=fx-core.example.com:443 \
//	    -partner-id=YOUR_PARTNER_ID \
//	    -api-key=YOUR_API_KEY \
//	    -dsn="postgres://user:pass@localhost:5432/fx?sslmode=disable" \
//	    -client-id=CLIENT_42 \
//	    -client-inn=123456789 \
//	    -tls-cert=client.crt \
//	    -tls-key=client.key \
//	    -tls-ca=ca.crt \
//	    -market
//
// -tls-cert и -tls-key включают mTLS: клиент предъявляет этот сертификат
// FX Core. -tls-ca проверяет сервер по приватному CA вместо системных корневых
// сертификатов; опустите его, если сертификат сервера публично доверенный.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
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
		target    = flag.String("target", "sdfif.tj:443", "FX Core gRPC address")
		sdkId     = flag.String("sdk-id", "01sdfb55359857", "SDK identifier")
		apiKey    = flag.String("api-key", "GsdsfFssKbyNuqn8ZnqAA==", "API key")
		dsn       = flag.String("dsn", "postgres://postgres:pass123@192.168.215.2:5432/fxdb?sslmode=disable", "Postgres DSN for the local orders table")
		partnerId = flag.String("partner-id", "019dfg8289ea", "partner identifier")
		clientId  = flag.String("client-id", "1278", "client identifier (required for filter)")
		clientINN = flag.String("client-inn", "07128328", "client INN (taxpayer ID)")
		insecureC = flag.Bool("insecure", false, "use plaintext gRPC (dev only)")
		cancel    = flag.Bool("cancel", false, "cancel the order after submission")
		market    = flag.Bool("market", false, "also submit a market order (priced by the book)")
		tlsCert   = flag.String("tls-cert", "test-c.crt", "client certificate PEM for mTLS")
		tlsKey    = flag.String("tls-key", "test-c.key", "client private key PEM for mTLS")
		// Намеренно пусто: сертификат сервера публично доверенный (ACM),
		// поэтому его проверяют системные корневые сертификаты. ca-bundle.pem —
		// это партнёрский CA fx, который выпустил только наш клиентский
		// сертификат, а не серверный.
		tlsCA = flag.String("tls-ca", "", "CA bundle PEM to verify the server; empty = system roots")
		mTLS  = flag.Bool("mTLS", false, "present the client certificate (-tls-cert/-tls-key)")
	)
	flag.Parse()

	if *sdkId == "" || *apiKey == "" || *dsn == "" || *clientId == "" || *clientINN == "" {
		log.Fatal("sdk-id, api-key, dsn, client-id and client-inn are required")
	}

	g, ctx := errgroup.WithContext(context.Background())
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	// Локальный пул Postgres, который SDK использует для учёта поданных ордеров.
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	// Собираем опции SDK. В продакшене используйте TLS (mTLS, если заданы
	// -tls-cert/-tls-key) вместо grpc.WithTransportCredentials(insecure.NewCredentials()).
	opts := []v1.Option{
		v1.WithMaxRetries(3),
		v1.WithRetryBackoff(100*time.Millisecond, 5*time.Second),
	}

	if *insecureC {
		opts = append(opts, v1.WithDialOptions(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		))
	} else if *mTLS {

		tlsCfg, err := loadTLSConfig(*tlsCert, *tlsKey, *tlsCA)
		if err != nil {
			log.Fatalf("tls: %v", err)
		}
		opts = append(opts, v1.WithDialOptions(
			grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
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

	// 0. Подписываемся на события ордеров в фоне.
	// Получает обновления в реальном времени при любом изменении статуса ордера.
	g.Go(func() error {
		log.Printf("starting order events subscription...")
		err := client.SubscribeOrderEvents(ctx, *partnerId, handleOrderEvent)
		if err != nil && ctx.Err() == nil {
			log.Printf("subscription error: %v", err)
		}
		return err
	})

	// 0b. Подписываемся на события сделок. SDK сохраняет сделку, считает
	// расчётную сумму и комиссию и обновляет ордер; обработчик ниже отвечает
	// за проводки по счетам, специфичные для партнёра.
	g.Go(func() error {
		log.Printf("starting trade subscription...")
		err := client.SubscribeTrades(ctx, handleTrade)
		if err != nil && ctx.Err() == nil {
			log.Printf("trade subscription error: %v", err)
		}
		return err
	})

	// 0c. При старте разбираем сделки, которые сохранены, но не рассчитаны
	// (сбой обработчика или падение после ack, но до расчёта), затем
	// продолжаем повторять их по таймеру. handleTrade должен быть идемпотентным.
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
	// 0c-bis. Сверяем завершённые часы с Core: при старте — каждый уже закрытый
	// час текущего дня, затем через несколько минут после каждого часа — только
	// что закрывшийся. При расхождении SDK забирает сделки Core за то же окно,
	// сохраняет локально всё недостающее и вызывает для него handleTrade, затем
	// проверяет повторно. Всё, что после этого всё ещё расходится, требует
	// участия человека — ReconcileDiff покажет, какие сделки разошлись.
	g.Go(func() error {
		reconcileHour := func(day string, hour int) {
			params := &v1.ReconcileParams{
				PartnerId: *partnerId,
				Day:       day,
				Hour:      hour,
			}
			res, err := client.ReconcileRepair(ctx, params, handleTrade)
			if err != nil {
				log.Printf("reconcile repair %s h%02d: %v", params.Day, params.Hour, err)
				return
			}
			if res.Resolved() {
				log.Printf("reconciled %s h%02d: recovered=%d settled=%d",
					params.Day, params.Hour, res.Recovered, res.Settled)
				return
			}
			log.Printf("reconcile %s h%02d STILL MISMATCHED: core=%d recovered=%d settled=%d settle_failed=%d failed=%d",
				params.Day, params.Hour, res.CoreTrades, res.Recovered,
				res.Settled, res.SettleFailed, res.Failed)
			diff, derr := client.ReconcileDiff(ctx, params)
			if derr != nil {
				log.Printf("reconcile diff %s h%02d: %v", params.Day, params.Hour, derr)
				return
			}
			log.Printf("  missing_locally=%d missing_remotely=%d mismatched=%d unsettled=%d",
				len(diff.MissingLocally), len(diff.MissingRemotely),
				len(diff.Mismatched), len(diff.Unsettled))
		}

		// Догоняем при старте, начиная с 00:00. Процесс, простоявший шесть часов,
		// оставляет шесть несверенных слотов, а не один, поэтому одного
		// почасового тика было бы мало — в таблице reconciliations остались бы
		// постоянные дыры. Каждый час записывается через upsert по (dt, hour),
		// так что повторный проход по уже записанным часам дёшев и идемпотентен —
		// совпавший час просто совпадёт снова.
		//
		// PreviousHour — последний завершённый час в часовом поясе SDK
		// (UTC+05:00). Не выводите его из часов хоста: Core сравнивает час в +05,
		// поэтому хост в другом поясе спросил бы про не то окно. Сразу после
		// полуночи он относится ко вчера, а у сегодняшнего дня ещё нет
		// завершённого часа, так что делать нужно только этот единственный час.
		day, hour := v1.PreviousHour()
		if day == v1.Today() {
			for h := range hour {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				reconcileHour(day, h)
			}
		}
		reconcileHour(day, hour)

		for {
			// Привязано к настенным часам, а не к старту процесса: запуск
			// приходится на hh:05 независимо от того, когда процесс стартовал.
			next := time.Now().In(v1.TimeZone).Truncate(time.Hour).Add(time.Hour + 5*time.Minute)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				day, hour := v1.PreviousHour()
				reconcileHour(day, hour)
			}
		}
	})

	// 0d. Получаем доступные валютные пары и их торговые спецификации.
	// Партнёр определяется по метаданным, поэтому параметры не нужны.
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
	// реквизиты счетов
	acc["debit_account"] = "1278"
	acc["credit_account"] = "4578"
	var fee = make(map[string]string)
	// фиксированная комиссия в процентах
	fee["fixed"] = "0.05"

	// 1. Подаём небольшой лимитный ордер на покупку USD/TJS по 9.31. OrderType
	// не задан, и SDK трактует это как v1.LimitOrder.
	submitted, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
		Side:             v1.Buy,
		Segment:          segment,
		PartnerId:        *partnerId,
		ClientId:         *clientId,
		ClientINN:        *clientINN,
		CurrencyPair:     "USD/TJS",
		Quantity:         "50000.00",
		LimitRate:        "10.00",
		MinTradeQuantity: "100.00",
		Account:          acc,
		Fee:              fee,
	})
	switch {
	case err == v1.ErrDuplicateOrder:
		log.Printf("submit: duplicate detected within 1 minute, skipping")
	case err != nil:
		log.Fatalf("submit order: %v", err)
	default:
		log.Printf("submitted: ref_id=%d order_id=%d order_day=%s status=%d cause=%q",
			submitted.RefId, submitted.OrderId, submitted.OrderDay, submitted.Status, submitted.Cause)
	}

	// 1b. Подаём рыночный ордер. У него нет LimitRate — цену определяет стакан,
	// а всё, что нельзя исполнить сразу, отменяется, а не остаётся в стакане.
	// Поскольку цена заранее неизвестна, ответ синхронно возвращает исполненное
	// количество и средневзвешенный курс; отдельные сделки по-прежнему
	// приходят по подписке на сделки.
	if *market {
		mkt, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
			Side:             v1.Sell,
			Segment:          segment,
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
			log.Printf("market submit: duplicate detected within 1 minute, skipping")
		case err != nil:
			log.Printf("market submit: %v", err)
		default:
			log.Printf("market submitted: ref_id=%d order_day=%s status=%d filled=%s avg_rate=%s cause=%q",
				mkt.RefId, mkt.OrderDay, mkt.Status, mkt.FilledQuantity, mkt.AverageRate, mkt.Cause)
		}
	}

	// 2. Фильтруем ордера клиента. ClientID обязателен; если OrderDayFrom/To
	// оба пусты, SDK подставляет сегодня и сегодня+1.
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
		log.Printf("  order_id=%d day=%s side=%d status=%d type=%d counterparty=%d qty=%s remaining=%s rate=%s ref=%d expires=%s",
			o.OrderId, o.OrderDay, o.Side, o.Status, o.OrderType, o.CounterpartySegment,
			o.Quantity, o.RemainingQuantity, o.LimitRate, o.RefId, o.ExpiresAt)
	}

	// 3. Получаем глубину стакана.
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

	// 4. Отменяем открытый ордер. CancelOrder нужен order_id из Core: его
	// возвращает SubmitOrder (и он хранится в client_orders.order_id), но здесь
	// для примера запрашиваем открытые ордера клиента через FilterClientOrders
	// и отменяем первый из них.
	if *cancel {
		open, err := client.FilterClientOrders(ctx, &v1.FilterClientOrdersParams{
			PartnerId:    *partnerId,
			ClientId:     *clientId,
			CurrencyPair: "USD/TJS",
			Status:       v1.Pending,
			Limit:        50,
		})
		if err != nil {
			log.Fatalf("filter open orders: %v", err)
		}
		if len(open.Orders) == 0 {
			log.Fatalf("cancel order: no open orders for client %s", *clientId)
		}
		// клиент выбирает, какой ордер отменить
		target := open.Orders[0]
		cancelled, err := client.CancelOrder(ctx, &v1.CancelOrderParams{
			PartnerId: *partnerId,
			ClientId:  *clientId,
			OrderId:   target.OrderId,
			OrderDay:  target.OrderDay,
		})
		if err != nil {
			log.Fatalf("cancel order: %v", err)
		}
		log.Printf("cancelled: success=%v remaining=%s status=%d cause=%q",
			cancelled.Success, cancelled.RemainingQuantity, cancelled.Status, cancelled.Cause)
	}

	// Ждём, чтобы фоновая подписка успела поймать последние события.
	if err := g.Wait(); err != nil {
		log.Fatalln("shutdown:", err)
	}
}

// loadTLSConfig собирает клиентский TLS-конфиг. Если заданы и certFile, и
// keyFile, включается mTLS; если задан caFile, он заменяет системные корневые
// сертификаты при проверке сервера.
func loadTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}

	switch {
	case certFile != "" && keyFile != "":
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	case certFile != "" || keyFile != "":
		return nil, errors.New("-tls-cert and -tls-key must be set together")
	}

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", caFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

func handleTrade(ctx context.Context, ev *v1.TradeEvent) error {
	log.Printf("TRADE: id=%d order=%d side=%d order_status=%d  day=%s filled=%s rate=%s settlement=%s fee=%s side=%d pair=%s trade_type=%d",
		ev.TradeId, ev.OrderId, ev.Side, ev.OrderStatus, ev.TradingDay, ev.FilledQuantity, ev.ExecutionRate,
		ev.Settlement, ev.Fee, ev.Side, ev.CurrencyPair, ev.TradeType)
	log.Printf("  accounts: debit=%s credit=%s -> move %s %s (fee %s)",
		ev.Account["debit_account"], ev.Account["credit_account"], ev.Settlement, ev.CurrencyPair, ev.Fee)
	return nil
}

func handleOrderEvent(event *v1.OrderEvent) {
	log.Printf("EVENT: ref_id=%d type=%d ts=%s remaining=%s",
		event.RefId, event.EventType, event.EventTimestamp, event.RemainingQuantity)
}
