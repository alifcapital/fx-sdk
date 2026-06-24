# Руководство разработчика по fx-sdk (Go)

SDK для интеграции партнёров с системой FX Core (форекс-торговля).
Предоставляет высокоуровневый gRPC-клиент для управления ордерами, получения
рыночных данных и обработки сделок (trades) с автоматическим учётом во
локальной базе данных партнёра.

- **Путь модуля:** `github.com/alifcapital/fx-sdk/go`
- **Рабочий пакет:** `github.com/alifcapital/fx-sdk/go/v1`

---

## Содержание

1. [Архитектура](#архитектура)
2. [Установка](#установка)
3. [Предварительные требования: база данных](#предварительные-требования-база-данных)
4. [Создание клиента](#создание-клиента)
5. [Транспорт и безопасность (TLS / mTLS)](#транспорт-и-безопасность-tls--mtls)
6. [Параметры конфигурации](#параметры-конфигурации)
7. [Отправка ордера — SubmitOrder](#отправка-ордера--submitorder)
8. [Отмена ордера — CancelOrder](#отмена-ордера--cancelorder)
9. [Фильтрация ордеров — FilterClientOrders](#фильтрация-ордеров--filterclientorders)
10. [Стакан цен — GetOrderBookDepth](#стакан-цен--getorderbookdepth)
11. [Подписка на события ордеров — SubscribeOrderEvents](#подписка-на-события-ордеров--subscribeorderevents)
12. [Подписка на сделки — SubscribeTrades](#подписка-на-сделки--subscribetrades)
13. [Повторная обработка незакрытых сделок — RetryUnsettled](#повторная-обработка-незакрытых-сделок--retryunsettled)
14. [Справочник типов и констант](#справочник-типов-и-констант)
15. [Обработка ошибок](#обработка-ошибок)
16. [Полный пример](#полный-пример)

---

## Архитектура

SDK оборачивает три gRPC-сервиса FX Core и синхронизирует их состояние с
**локальной базой данных партнёра** (PostgreSQL / TimescaleDB):

| Сервис | Назначение |
|--------|-----------|
| `OrderService` | Жизненный цикл ордеров (отправка, отмена, стакан, фильтр, события) |
| `TradeService` | Поток исполненных сделок (двунаправленный стрим с подтверждением) |
| `PartnerService` | Справочные данные (валютные пары) |

Ключевые особенности:

- **Локальный учёт.** Каждый ордер сначала записывается в таблицу `client_orders`,
  и только потом отправляется в Core. `ref_id` (идемпотентный ключ) генерируется
  базой как `BIGSERIAL`.
- **Идемпотентность.** SDK защищает от дублей: одинаковый ордер в пределах
  2 минут отклоняется (`ErrDuplicateOrder`).
- **Надёжность стримов.** Подписки автоматически переподключаются с
  экспоненциальной задержкой (backoff) при временных сбоях.
- **Гарантия расчёта сделок.** Сделка сначала надёжно сохраняется в БД, затем
  подтверждается в Core (ack), и только потом выполняется расчёт по счетам
  партнёра. Незавершённые расчёты переигрываются через `RetryUnsettled`.
- **Десятичная точность.** Все денежные значения — это **строки** (`string`),
  чтобы избежать потери точности при работе с числами с плавающей точкой.

---

## Установка

```bash
go get github.com/alifcapital/fx-sdk/go@latest
```

Импорт в коде:

```go
import (
    v1 "github.com/alifcapital/fx-sdk/go/v1"
)
```

---

## Предварительные требования: база данных

SDK хранит ордера и сделки в **локальной БД партнёра**. Перед использованием
необходимо создать таблицы. Полная схема находится в файле [`go/db.sql`](go/db.sql).

Требуется **PostgreSQL с расширением TimescaleDB** (таблицы создаются как
гипертаблицы с партиционированием по дню).

Основные таблицы:

- `client_orders` — все отправленные ордера. Первичный ключ `(ref_id, order_day)`.
- `client_trades` — исполненные сделки. Первичный ключ `(trade_id, order_id, trading_day)`.
- `reconciliations` — данные для сверки с Core (опционально).

Создание схемы:

```bash
psql "postgres://user:pass@host:5432/fxdb" -f go/db.sql
```

> **Важно:** SDK выполняет SQL-запросы напрямую к этим таблицам. Имена и
> структура колонок менять нельзя.

---

## Создание клиента

Клиент создаётся функцией `v1.New`. Она требует адрес Core, идентификатор SDK,
API-ключ, идентификатор партнёра и пул соединений с локальной БД (`*pgxpool.Pool`).

```go
import (
    "context"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"

    v1 "github.com/alifcapital/fx-sdk/go/v1"
)

func newClient(ctx context.Context) (*v1.Client, error) {
    // Пул соединений с локальной БД партнёра.
    pool, err := pgxpool.New(ctx, "postgres://user:pass@localhost:5432/fxdb?sslmode=disable")
    if err != nil {
        return nil, err
    }

    opts := []v1.Option{
        v1.WithMaxRetries(3),
        v1.WithRetryBackoff(100*time.Millisecond, 5*time.Second),
        // DEV: незащищённое (plaintext) соединение — только для разработки.
        // PROD: используйте mTLS — см. раздел «Транспорт и безопасность».
        v1.WithDialOptions(
            grpc.WithTransportCredentials(insecure.NewCredentials()),
        ),
    }

    client, err := v1.New(
        "fx-core.example.com:443", // адрес Core
        "019eee39-cc7f-722e-a3f1-c2c010b141a4", // sdk_id (ровно 36 символов, UUID)
        "ВАШ_API_КЛЮЧ",                          // api_key
        "019eee2d-d765-7273-8582-ab6982339896",  // partner_id
        pool,
        opts...,
    )
    if err != nil {
        return nil, err
    }
    return client, nil
}
```

Параметры `New(target, sdkId, apiKey, partnerId, db, opts...)`:

| Параметр | Описание | Валидация |
|----------|----------|-----------|
| `target` | Адрес gRPC-сервера Core | не пустой |
| `sdkId` | Идентификатор SDK | строго 36 символов (UUID) |
| `apiKey` | API-ключ партнёра | не пустой |
| `partnerId` | Идентификатор партнёра |
| `db` | Пул соединений `*pgxpool.Pool` | не `nil` |

`sdkId`, `apiKey` и `partnerId` автоматически добавляются в gRPC-метаданные
(`key-id`, `api-key`, `partner-id`) каждого запроса.

`partnerId` - идентификатор партнёра для каждого запроса.

> Не забудьте вызвать `defer client.Close()` для освобождения соединения.

---

## Транспорт и безопасность (TLS / mTLS)

Тип транспорта задаётся через `WithDialOptions` и **различается между окружениями**:

| Окружение | Транспорт | Учётные данные |
|-----------|-----------|----------------|
| **DEV** (разработка) | Незащищённый (plaintext), без mTLS | `insecure.NewCredentials()` |
| **PROD** (продакшен) | **mTLS** (взаимная аутентификация по сертификатам) | клиентский сертификат + ключ + CA |

### DEV — без mTLS

В среде разработки Core принимает незащищённое (plaintext) соединение:

```go
import "google.golang.org/grpc/credentials/insecure"

opts := []v1.Option{
    v1.WithDialOptions(
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    ),
}
```

> Используйте `insecure` **только** в разработке. Никогда не подключайтесь к
> продакшену без TLS — API-ключ уйдёт по сети в открытом виде.

### PROD — mTLS

В продакшене Core требует **взаимный TLS (mTLS)**: помимо проверки сертификата
сервера, клиент обязан предъявить собственный сертификат, подписанный доверенным
CA. Партнёр получает клиентский сертификат (`client.crt`), приватный ключ
(`client.key`) и корневой сертификат CA (`ca.crt`).

```go
import (
    "crypto/tls"
    "crypto/x509"
    "fmt"
    "os"

    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials"

    v1 "github.com/alifcapital/fx-sdk/go/v1"
)

func mtlsDialOption(certFile, keyFile, caFile, serverName string) (grpc.DialOption, error) {
    // Клиентская пара сертификат/ключ — её предъявляем серверу.
    cert, err := tls.LoadX509KeyPair(certFile, keyFile)
    if err != nil {
        return nil, fmt.Errorf("загрузка клиентского сертификата: %w", err)
    }

    // CA, которым подписан сертификат сервера, — для проверки сервера.
    caPEM, err := os.ReadFile(caFile)
    if err != nil {
        return nil, fmt.Errorf("чтение CA: %w", err)
    }
    pool := x509.NewCertPool()
    if !pool.AppendCertsFromPEM(caPEM) {
        return nil, fmt.Errorf("не удалось разобрать CA-сертификат")
    }

    tlsCfg := &tls.Config{
        Certificates: []tls.Certificate{cert}, // mTLS: предъявляем свой сертификат
        RootCAs:      pool,                     // проверяем сертификат сервера
        ServerName:   serverName,               // должен совпадать с CN/SAN сервера
        MinVersion:   tls.VersionTLS12,
    }
    return grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), nil
}
```

Использование при создании клиента:

```go
dialOpt, err := mtlsDialOption(
    "/etc/fx-sdk/client.crt",
    "/etc/fx-sdk/client.key",
    "/etc/fx-sdk/ca.crt",
    "fx-core.example.com", // имя из сертификата сервера
)
if err != nil {
    log.Fatalf("настройка mTLS: %v", err)
}

opts := []v1.Option{
    v1.WithMaxRetries(3),
    v1.WithRetryBackoff(100*time.Millisecond, 5*time.Second),
    v1.WithDialOptions(dialOpt),
}

client, err := v1.New(
    "fx-core.example.com:443",
    sdkID, apiKey, partnerID, pool,
    opts...,
)
```

Рекомендации для продакшена:

- Храните `client.key` с правами `0600`, вне репозитория (секрет-хранилище).
- `ServerName` должен совпадать с CN/SAN в сертификате сервера, иначе рукопожатие
  (handshake) не пройдёт.
- Отслеживайте срок действия сертификатов и обновляйте их заранее.
- Не используйте `InsecureSkipVerify: true` — это отключает проверку сервера и
  сводит на нет смысл mTLS.

---

## Параметры конфигурации

Опции передаются в `v1.New` через вариативный аргумент `opts ...Option`:

| Опция | Назначение | Значение по умолчанию |
|-------|-----------|----------------------|
| `WithMaxRetries(n int)` | Кол-во повторов для временных ошибок gRPC (`Unavailable`, `ResourceExhausted`) | 3 |
| `WithRetryBackoff(base, max time.Duration)` | Базовая и максимальная задержка экспоненциального backoff | 100ms / 5s |
| `WithDialOptions(opts ...grpc.DialOption)` | Дополнительные опции подключения gRPC (например, TLS) | — |

Backoff применяется как к unary-вызовам, так и к переподключению стримов.
Для долгоживущих стримов SDK также включает gRPC keepalive (пинг каждые 30 секунд),
чтобы прокси и NAT не разрывали простаивающее соединение.

---

## Отправка ордера — SubmitOrder

Размещает новый ордер. SDK сначала вставляет строку в `client_orders`
(генерируя `ref_id` и `order_day`), затем отправляет ордер в Core и обновляет
локальную строку статусом из ответа.

```go
acc := map[string]string{
    "debit_account":  "1271",
    "credit_account": "4571",
}
fee := map[string]string{
    "fixed": "0.05", // комиссия в процентах (0.05%)
}

res, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
    Side:             v1.Buy,        // Buy или Sell
    Segment:          v1.Retail,     // Retail / Corporate / Treasury
    AllowPartialFill: true,
    PartnerId:        partnerId,
    ClientId:         "1271",
    ClientINN:        "07128321",    // ИНН клиента
    CurrencyPair:     "USD/TJS",
    Quantity:         "1000.00",     // десятичная строка
    LimitRate:        "9.31",        // десятичная строка
    MinTradeQuantity: "100.00",      // мин. объём частичного исполнения
    Account:          acc,           // произвольный JSONB
    Fee:              fee,           // произвольный JSONB
})
if err != nil {
    if errors.Is(err, v1.ErrDuplicateOrder) {
        log.Println("дубликат ордера в пределах 2 минут — пропускаем")
        return
    }
    log.Fatalf("ошибка отправки ордера: %v", err)
}

log.Printf("ref_id=%d order_day=%s status=%d cause=%q",
    res.RefId, res.OrderDay, res.Status, res.Cause)
```

**Обязательные поля:** `ClientId`, `PartnerId`, `Segment` (должен быть в диапазоне
`Retail..Treasury`).

**Результат `SubmitOrderResult`:**

| Поле | Описание |
|------|----------|
| `RefId` | Локальный идемпотентный ключ (используется для отмены и сопоставления) |
| `OrderDay` | Дата ордера в формате `YYYY-MM-DD` |
| `Status` | Статус, возвращённый Core (см. `OrderStatus`) |
| `Cause` | Причина отклонения/ошибки, если есть |

> **Защита от дублей:** если ордер с теми же `side`, `limit_rate`, `quantity`,
> `currency_pair`, `partner_id`, `client_id` был отправлен за последние 2 минуты,
> возвращается `v1.ErrDuplicateOrder`.

---

## Отмена ордера — CancelOrder

Отменяет существующий ордер. Требует `OrderId` (это локальный `RefId` из
`SubmitOrder`) и `OrderDay`.

```go
res, err := client.CancelOrder(ctx, &v1.CancelOrderParams{
    PartnerId: partnerId,
    ClientId:  "1271",
    OrderId:   submitted.RefId,   // ref_id из SubmitOrder
    OrderDay:  submitted.OrderDay,
})
if err != nil {
    log.Fatalf("ошибка отмены: %v", err)
}

log.Printf("success=%v remaining=%s status=%d cause=%q",
    res.Success, res.RemainingQuantity, res.Status, res.Cause)
```

**Результат `CancelOrderResult`:**

| Поле | Описание |
|------|----------|
| `Success` | Успешно ли отменён ордер |
| `RemainingQuantity` | Невыполненный объём на момент отмены (для возврата средств) |
| `Status` | Обновлённый статус ордера |
| `Cause` | Причина неудачи, если есть |
| `RefId` | Локальный `ref_id`, связанный с ордером |

При успешной отмене SDK автоматически обновляет статус и остаток в `client_orders`.

---

## Фильтрация ордеров — FilterClientOrders

Запрашивает у Core ордера клиента по фильтрам с пагинацией.

```go
res, err := client.FilterClientOrders(ctx, &v1.FilterClientOrdersParams{
    PartnerId:    partnerId,
    ClientId:     "1271",          // обязательно
    CurrencyPair: "USD/TJS",       // опционально
    Side:         v1.Buy,          // опционально (0 = не задано)
    Status:       v1.Pending,      // опционально (0 = не задано)
    OrderDayFrom: "2026-06-01",    // YYYY-MM-DD; по умолчанию сегодня
    OrderDayTo:   "2026-06-30",    // YYYY-MM-DD; по умолчанию сегодня+1
    Limit:        50,              // максимум 100
    Offset:       0,
})
if err != nil {
    log.Fatalf("ошибка фильтра: %v", err)
}

for _, o := range res.Orders {
    log.Printf("order_id=%d day=%s side=%d status=%d qty=%s remaining=%s rate=%s ref=%d",
        o.OrderId, o.OrderDay, o.Side, o.Status,
        o.Quantity, o.RemainingQuantity, o.LimitRate, o.RefId)
}
```

**Обязательные поля:** `ClientId`, `PartnerId`.

Если `OrderDayFrom` или `OrderDayTo` пусты, SDK подставляет сегодня и сегодня+1
соответственно. Каждый элемент `res.Orders` имеет тип `v1.Order`
(см. [справочник типов](#справочник-типов-и-констант)).

---

## Стакан цен — GetOrderBookDepth

Возвращает агрегированный стакан (заявки на покупку `Bids` и на продажу `Asks`)
для валютной пары.

```go
depth, err := client.GetOrderBookDepth(ctx, &v1.GetOrderBookDepthParams{
    Segment:      v1.Retail,
    MaxLevels:    10,          // максимальное число ценовых уровней
    ClientId:     "1271",
    CurrencyPair: "USD/TJS",
    PartnerId:    partnerId,
})
if err != nil {
    log.Fatalf("ошибка стакана: %v", err)
}

log.Println("Asks (продажа):")
for i := len(depth.Asks) - 1; i >= 0; i-- {
    log.Printf("  %s: %s", depth.Asks[i].Rate, depth.Asks[i].TotalQuantity)
}
log.Println("Bids (покупка):")
for _, b := range depth.Bids {
    log.Printf("  %s: %s", b.Rate, b.TotalQuantity)
}
```

Каждый уровень `v1.PriceLevel` содержит `Rate` (цена) и `TotalQuantity`
(суммарный объём), обе — десятичные строки.

**Обязательные поля:** `ClientId`, `PartnerId`, корректный `Segment`.

---

## Подписка на события ордеров — SubscribeOrderEvents

Серверный стрим: получает изменения статусов ордеров в реальном времени.
При получении события SDK обновляет локальную БД и вызывает ваш обработчик.

```go
func handleOrderEvent(event *v1.OrderEvent) {
    log.Printf("СОБЫТИЕ: ref_id=%d type=%d ts=%s remaining=%s",
        event.RefId, event.EventType, event.EventTimestamp, event.RemainingQuantity)

    // Если событие — отмена или истечение срока, нужно вернуть средства.
    if amount, release := event.GetReleaseAmount(); release {
        log.Printf("освободить средства: %s", amount)
    }
}

// Запускайте в отдельной горутине — вызов блокирующий.
go func() {
    err := client.SubscribeOrderEvents(ctx, partnerId, handleOrderEvent)
    if err != nil && ctx.Err() == nil {
        log.Printf("подписка завершилась с ошибкой: %v", err)
    }
}()
```

**Поведение переподключения:** стрим автоматически переоткрывается при временных
ошибках (`Unavailable`, `ResourceExhausted`) и чистом `EOF` от сервера, с задержкой
backoff между попытками. Счётчик неудач **сбрасывается при каждом успешном
получении сообщения**, поэтому работающая подписка переживает сколько угодно
переподключений. Метод возвращает управление только когда:

- контекст `ctx` отменён;
- ошибка не подлежит повтору;
- исчерпано `maxRetries` подряд неудачных попыток переподключения.

`OrderEvent.GetReleaseAmount()` удобно использовать для возврата средств: он
возвращает `(остаток, true)` для статусов `Expired`, `ExpiredPartially`,
`Cancelled`, `CancelledPartially`.

---

## Подписка на сделки — SubscribeTrades

Двунаправленный стрим, реализующий полный цикл расчёта по сделке. Для каждого
события Core SDK:

1. Находит родительский ордер по `ref_id` (берёт `client_id`, `side`,
   `currency_pair`, `account`, конфигурацию комиссии).
2. Вставляет сделку в `client_trades`.
3. Подтверждает сделку Core (**ack** = «получено и сохранено»).
4. Обновляет ордер: уменьшает `remaining_quantity` и ставит новый статус.
5. Вызывает ваш обработчик для расчёта по счетам партнёра (дебет/кредит).

```go
func handleTrade(ctx context.Context, ev *v1.TradeEvent) error {
    // ev.Settlement и ev.Fee уже вычислены SDK.
    log.Printf("СДЕЛКА: id=%d order=%d filled=%s rate=%s settlement=%s fee=%s pair=%s",
        ev.TradeId, ev.OrderId, ev.FilledQuantity, ev.ExecutionRate,
        ev.Settlement, ev.Fee, ev.CurrencyPair)

    // Перемещение средств по счетам партнёра.
    // ВАЖНО: операция ДОЛЖНА быть идемпотентной — одна и та же сделка
    // (trade_id, order_id, trading_day) может прийти повторно.
    return moveFunds(ctx, ev.Account["debit_account"],
        ev.Account["credit_account"], ev.Settlement, ev.Fee)
}

go func() {
    err := client.SubscribeTrades(ctx, handleTrade)
    if err != nil && ctx.Err() == nil {
        log.Printf("подписка на сделки завершилась с ошибкой: %v", err)
    }
}()
```

**Разделение состояний** (критично для денежных операций):

- **Получено и сохранено** — сделка надёжно записана в БД. Только после этого
  отправляется ack в Core. Если сохранить не удалось, ack не отправляется и
  Core доставит сделку повторно.
- **Рассчитано (settled)** — обработчик партнёра успешно выполнил перемещение
  средств. Отслеживается отдельно в колонке `settled`. Если обработчик вернул
  ошибку, сделка остаётся `settled = FALSE` с записанной ошибкой, и её повторит
  `RetryUnsettled`.

> **Требование идемпотентности.** Обработчик `TradeEventHandler` ДОЛЖЕН быть
> идемпотентным. Одна и та же сделка может быть представлена более одного раза —
> при повторной доставке после переподключения или при повторе после сбоя.
> Перемещение средств должно становиться no-op, если оно уже было применено
> для этой сделки.

Поля `TradeEvent` (наиболее важные):

| Поле | Описание |
|------|----------|
| `TradeId`, `OrderId` | Идентификаторы сделки и ордера в Core |
| `TradingDay` | День сделки `YYYY-MM-DD` |
| `FilledQuantity` | Исполненный объём в базовой валюте |
| `ExecutionRate` | Курс исполнения |
| `Side` | Направление (`Buy` / `Sell`) |
| `Account` | JSONB-счета из родительского ордера (`map[string]string`) |
| `FeeConfig` | JSONB-конфигурация комиссии |
| `Settlement` | Итоговая сумма расчёта (`decimal.Decimal`), уже вычислена |
| `Fee` | Комиссия (`decimal.Decimal`), уже вычислена |

**Расчёт `Settlement` и `Fee`** (выполняется методом `TradeEvent.Cal()` внутри SDK):

```
m   = filled_quantity * execution_rate
fee = m * fee_config["fixed"] / 100          (комиссия в процентах)

Settlement = m + fee   (если Side == Buy)
Settlement = m - fee   (если Side == Sell)
```

Все результаты округляются до 6 знаков после запятой.

---

## Повторная обработка незакрытых сделок — RetryUnsettled

Переигрывает обработчик расчёта для всех сделок, которые сохранены, но ещё не
рассчитаны (`settled = FALSE`) — например, обработчик упал, или процесс
завершился после ack, но до расчёта.

Источник истины для «деньги ещё нужно переместить» — это локальный флаг
`settled`, а не повторная доставка от Core. Поэтому `RetryUnsettled` гарантирует,
что неудавшийся расчёт будет в итоге повторён даже на здоровом, никогда не
переподключающемся стриме.

**Вызывайте при старте приложения и по таймеру:**

```go
// При старте.
if err := client.RetryUnsettled(ctx, handleTrade); err != nil {
    log.Printf("retry unsettled (старт): %v", err)
}

// По таймеру.
go func() {
    t := time.NewTicker(time.Minute)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            if err := client.RetryUnsettled(ctx, handleTrade); err != nil {
                log.Printf("retry unsettled: %v", err)
            }
        }
    }
}()
```

> Обработчик так же ДОЛЖЕН быть идемпотентным — сделка может быть представлена
> ему повторно.

---

## Справочник типов и констант

### Направление ордера — `Side`

```go
v1.Buy  // 1 — покупка
v1.Sell // 2 — продажа
```

### Сегмент рынка — `Segment`

```go
v1.Retail    // 1 — розница
v1.Corporate // 2 — корпоративный
v1.Treasury  // 3 — казначейство
```

### Статус ордера — `OrderStatus`

| Константа | Значение | Описание |
|-----------|----------|----------|
| `Unknown` | 0 | Неизвестно |
| `Pending` | 1 | В ожидании |
| `FilledPartially` | 2 | Частично исполнен |
| `Filled` | 3 | Полностью исполнен |
| `Cancelled` | 4 | Отменён |
| `Expired` | 5 | Истёк срок |
| `Failed` | 6 | Ошибка |
| `CancelledPartially` | 7 | Частично отменён |
| `ExpiredPartially` | 8 | Частично истёк |
| `Rejected` | 9 | Отклонён |
| `Duplicate` | 10 | Дубликат |

### Денежные значения

Все количества, курсы и суммы — **строки** для произвольной десятичной точности:
`"1000.00"`, `"9.3100"`. В `TradeEvent` поля `Settlement` и `Fee` имеют тип
`decimal.Decimal` (`github.com/govalues/decimal`).

---

## Обработка ошибок

Предопределённые ошибки SDK (сравнивайте через `errors.Is`):

| Ошибка | Когда возникает |
|--------|-----------------|
| `v1.ErrDuplicateOrder` | Дубликат ордера в пределах 2 минут |
| `v1.ErrClientIDRequired` | Не указан `client_id` |
| `v1.ErrPartnerIDRequired` | Не указан `partner_id` |

```go
res, err := client.SubmitOrder(ctx, params)
switch {
case errors.Is(err, v1.ErrDuplicateOrder):
    // дубликат — обычно безопасно пропустить
case errors.Is(err, v1.ErrClientIDRequired):
    // ошибка валидации параметров
case err != nil:
    // сетевые/серверные ошибки (gRPC). Временные ошибки уже
    // повторены внутри SDK согласно WithMaxRetries.
default:
    // успех
}
```

Временные ошибки gRPC (`Unavailable`, `ResourceExhausted`) автоматически
повторяются для unary-вызовов и вызывают переподключение для стримов.

---

## Полный пример

Полный рабочий пример с подписками, отправкой, фильтрацией и отменой ордера
находится в [`go/example/main.go`](go/example/main.go). Запуск:

```bash
cd go && go run ./example \
    -target=fx-core.example.com:443 \
    -sdk-id=YOUR_SDK_ID \
    -api-key=YOUR_API_KEY \
    -partner-id=YOUR_PARTNER_ID \
    -dsn="postgres://user:pass@localhost:5432/fxdb?sslmode=disable" \
    -client-id=CLIENT_42 \
    -client-inn=123456789
```

Рекомендуемая структура продакшен-приложения (через `errgroup`):

```go
g, ctx := errgroup.WithContext(context.Background())
ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
defer stop()

// 1. Подписка на события ордеров.
g.Go(func() error {
    return client.SubscribeOrderEvents(ctx, partnerId, handleOrderEvent)
})

// 2. Подписка на сделки.
g.Go(func() error {
    return client.SubscribeTrades(ctx, handleTrade)
})

// 3. Переигрывание незакрытых расчётов при старте и по таймеру.
g.Go(func() error {
    _ = client.RetryUnsettled(ctx, handleTrade)
    t := time.NewTicker(time.Minute)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-t.C:
            _ = client.RetryUnsettled(ctx, handleTrade)
        }
    }
})

// ... бизнес-логика (SubmitOrder, CancelOrder, ...) ...

if err := g.Wait(); err != nil {
    log.Fatalln("завершение:", err)
}
```

---

## Чек-лист интеграции

- [ ] Создана схема БД из [`go/db.sql`](go/db.sql) (PostgreSQL + TimescaleDB).
- [ ] Получены `sdk_id` (36-символьный UUID), `api_key` и `partner_id`.
- [ ] DEV: подключение через `insecure` (без mTLS). PROD: настроен **mTLS**
      (клиентский сертификат + ключ + CA) через `WithDialOptions`.
- [ ] Запущены подписки `SubscribeOrderEvents` и `SubscribeTrades` в горутинах.
- [ ] Настроен `RetryUnsettled` при старте и по таймеру.
- [ ] Обработчик сделок `handleTrade` сделан **идемпотентным**.
- [ ] Реализован возврат средств по `OrderEvent.GetReleaseAmount()`.
- [ ] Денежные значения передаются как строки с нужной точностью.
- [ ] Вызывается `client.Close()` при завершении.
