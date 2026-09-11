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
4. [Часовой пояс — UTC+05:00](#часовой-пояс--utc0500)
5. [Создание клиента](#создание-клиента)
6. [Транспорт и безопасность (TLS / mTLS)](#транспорт-и-безопасность-tls--mtls)
7. [Параметры конфигурации](#параметры-конфигурации)
8. [Отправка ордера — SubmitOrder](#отправка-ордера--submitorder)
9. [Отмена ордера — CancelOrder](#отмена-ордера--cancelorder)
10. [Фильтрация ордеров — FilterClientOrders](#фильтрация-ордеров--filterclientorders)
11. [Стакан цен — GetOrderBookDepth](#стакан-цен--getorderbookdepth)
12. [Валютные пары — GetCurrencyPairs](#валютные-пары--getcurrencypairs)
13. [Подписка на события ордеров — SubscribeOrderEvents](#подписка-на-события-ордеров--subscribeorderevents)
14. [Подписка на сделки — SubscribeTrades](#подписка-на-сделки--subscribetrades)
15. [Повторная обработка незакрытых сделок — RetryUnsettled](#повторная-обработка-незакрытых-сделок--retryunsettled)
16. [Восстановление после простоя — RecoverTrades](#восстановление-после-простоя--recovertrades)
17. [Сверка с Core — Reconcile, ReconcileDiff, ReconcileRepair](#сверка-с-core--reconcile-reconcilediff-reconcilerepair)
18. [Справочник типов и констант](#справочник-типов-и-констант)
19. [Обработка ошибок](#обработка-ошибок)
20. [Полный пример](#полный-пример)

---

## Архитектура

SDK оборачивает три gRPC-сервиса FX Core и синхронизирует их состояние с
**локальной базой данных партнёра** (PostgreSQL / TimescaleDB):

| Сервис | Назначение |
|--------|-----------|
| `OrderService` | Жизненный цикл ордеров (отправка, отмена, стакан, фильтр, события) |
| `TradeService` | Поток исполненных сделок (двунаправленный стрим с подтверждением) |
| `PartnerService` | Справочные данные (валютные пары и их торговые характеристики) |

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

## Часовой пояс — UTC+05:00

**FX Core работает в UTC+05:00** (время Таджикистана, перехода на летнее время
нет). Все даты и часы, которые Core присылает, ожидает и сравнивает, заданы в
этом смещении, поэтому SDK привязан к нему же. Это относится к `order_day`,
`trading_day`, к границам окна сверки и к параметрам `Day` / `Hour` у
`Reconcile`.

Смещение задано жёстко и не настраивается: его определяет Core, а не партнёр.

**Часовой пояс сессии PostgreSQL и часовой пояс хоста не имеют значения.** SDK
не полагается на настройку `TimeZone`: каждое сравнение `TIMESTAMPTZ` с датой
привязывается к смещению явно, через `AT TIME ZONE INTERVAL '+05:00'`.
Настраивать пул подключений не нужно.

### Почему это важно

Две вещи ломаются, если дата берётся из сессии:

1. **Сверка.** `Reconcile` сравнивает контрольную сумму по локальным сделкам за
   один час с контрольной суммой Core за тот же час. Core считает час в +05, и
   суммы совпадут только если обе стороны имеют в виду один и тот же час.
   Партнёр с сессией в UTC запросил бы окно, сдвинутое на пять часов, — и
   **каждый** час расходился бы, хотя данные совпадают.
2. **`order_day` около полуночи.** Колонка входит в первичный ключ
   `client_orders` и является тем значением, по которому сделка связывается со
   своим ордером. В 02:30 по +05 сессия в UTC записала бы предыдущий день
   (21:30), после чего Core не нашёл бы ордер по `order_day`.

### Ловушка со знаком

В SQL используйте форму `INTERVAL`, а не строку. PostgreSQL трактует
`AT TIME ZONE '+05'` по инвертированному соглашению POSIX:

```sql
SELECT TIMESTAMP '2026-04-30 10:00:00' AT TIME ZONE '+05';               -- 15:00Z — неверно
SELECT TIMESTAMP '2026-04-30 10:00:00' AT TIME ZONE INTERVAL '+05:00';   -- 05:00Z — верно
SELECT TIMESTAMP '2026-04-30 10:00:00' AT TIME ZONE 'Asia/Dushanbe';     -- 05:00Z — верно
```

Ошибка в знаке сдвигает окно сверки на десять часов и ничем себя не проявляет.

### Что использовать в своём коде

Не берите день и час из `time.Now()` — часы хоста могут идти в другом поясе:

```go
// Текущий торговый день в поясе SDK.
day := v1.Today()

// Последний ЗАВЕРШЁННЫЙ час — именно его нужно передавать в Reconcile.
// Корректно переходит через полночь: в 00:30 пятого числа вернёт
// час 23 четвёртого.
day, hour := v1.PreviousHour()

res, err := client.ReconcileRepair(ctx, &v1.ReconcileParams{
    PartnerId: partnerId,
    Day:       day,
    Hour:      hour,
}, handleTrade)

// Если нужен свой расчёт — используйте пояс SDK, а не time.Local.
t := time.Now().In(v1.TimeZone)
```

| Идентификатор     | Назначение                                                   |
| ----------------- | ------------------------------------------------------------ |
| `v1.TimeZone`     | Фиксированный пояс UTC+05:00 (`*time.Location`)              |
| `v1.Today()`      | Текущий торговый день в этом поясе, `YYYY-MM-DD`             |
| `v1.PreviousHour()` | День и час последнего завершённого часа                    |

В собственных запросах к `client_orders` и `client_trades` привязывайте
`TIMESTAMPTZ` так же, как это делает SDK, иначе ваши отчёты не совпадут с
результатами сверки:

```sql
-- сделки за час 10 по +05
SELECT *
  FROM client_trades
 WHERE trading_day = '2026-04-30'::date
   AND executed_at >= TIMESTAMP '2026-04-30 10:00:00' AT TIME ZONE INTERVAL '+05:00'
   AND executed_at <  TIMESTAMP '2026-04-30 11:00:00' AT TIME ZONE INTERVAL '+05:00';

-- вывод времени в поясе SDK, а не сессии
SELECT to_char(executed_at AT TIME ZONE INTERVAL '+05:00', 'YYYY-MM-DD HH24:MI:SS')
  FROM client_trades;
```

Колонки времени остаются `TIMESTAMPTZ` и хранят однозначный момент времени —
привязка влияет только на то, к какому календарному дню и к какому часу этот
момент отнесён.

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
    LimitRate:        "9.31",        // десятичная строка (обязателен для лимитного ордера)
    MinTradeQuantity: "100.00",      // мин. объём частичного исполнения
    Account:          acc,           // произвольный JSONB
    Fee:              fee,           // произвольный JSONB
    OrderType:        v1.LimitOrder, // 0 или v1.LimitOrder — лимитный; v1.MarketOrder — рыночный
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
`Retail..Treasury`), `LimitRate` — **только для лимитного ордера**.

**Результат `SubmitOrderResult`:**

| Поле | Описание |
|------|----------|
| `RefId` | Локальный идемпотентный ключ (используется для отмены и сопоставления) |
| `OrderDay` | Дата ордера в формате `YYYY-MM-DD` |
| `Status` | Статус, возвращённый Core (см. `OrderStatus`) |
| `Cause` | Причина отклонения/ошибки, если есть |
| `FilledQuantity` | Исполненный объём — **только для рыночного ордера**, иначе пусто |
| `AverageRate` | Средневзвешенный курс исполнения — **только для рыночного ордера**, иначе пусто |

> **Защита от дублей:** если ордер с теми же `side`, `limit_rate`, `quantity`,
> `currency_pair`, `order_type`, `partner_id`, `client_id` был отправлен за
> последние 2 минуты, возвращается `v1.ErrDuplicateOrder`.

### Тип ордера — лимитный и рыночный

| `OrderType` | Цена | Неисполненный остаток |
|-------------|------|-----------------------|
| `v1.LimitOrder` (1, по умолчанию) | `LimitRate` или лучше | остаётся в стакане до исполнения, истечения срока или отмены |
| `v1.MarketOrder` (2) | лучшие доступные цены стакана | **отменяется**, в стакане ничего не остаётся |

Рыночный ордер **игнорирует `LimitRate`** — SDK не отправляет это поле в Core и
записывает в `client_orders.limit_rate` значение `NULL`. Курс заранее неизвестен,
поэтому исполнение возвращается **синхронно** в `FilledQuantity` и `AverageRate`:

```go
res, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
    Side:             v1.Buy,
    Segment:          v1.Retail,
    AllowPartialFill: true,
    PartnerId:        partnerId,
    ClientId:         "1271",
    ClientINN:        "07128321",
    CurrencyPair:     "USD/TJS",
    Quantity:         "100.00",
    OrderType:        v1.MarketOrder, // LimitRate не указывается
    Account:          acc,
    Fee:              fee,
})
if err != nil {
    log.Fatalf("ошибка отправки рыночного ордера: %v", err)
}

log.Printf("исполнено %s по среднему курсу %s (статус %d)",
    res.FilledQuantity, res.AverageRate, res.Status)
```

> **Важно:** `FilledQuantity` / `AverageRate` — это сводка для немедленного
> отображения курса клиенту. Сами сделки всё равно приходят в подписке
> `SubscribeTrades`, и именно она остаётся источником истины для расчётов: SDK
> уменьшает `remaining_quantity` только по событиям сделок, поэтому один и тот же
> объём никогда не учитывается дважды.

### Ограничение контрагента — `CounterpartySegment`

По умолчанию (`v1.AnyCounterparty`, значение `0`) ордер может встретиться с
контрагентом из любого сегмента. Ордер сегмента `Treasury` может дополнительно
потребовать сводить его **только с казначейскими контрагентами**:

```go
res, err := client.SubmitOrder(ctx, &v1.SubmitOrderParams{
    Segment:             v1.Treasury,
    CounterpartySegment: v1.Treasury, // только казначейские контрагенты
    // ... остальные поля
})
```

Любое другое значение отклоняется с `v1.ErrInvalidCounterpartySegment`, а попытка
задать `Treasury` для ордера другого сегмента — с `v1.ErrTreasuryCounterpartyOnly`.

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
    log.Printf("order_id=%d day=%s side=%d status=%d type=%d counterparty=%d qty=%s remaining=%s rate=%s ref=%d",
        o.OrderId, o.OrderDay, o.Side, o.Status, o.OrderType, o.CounterpartySegment,
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

## Валютные пары — GetCurrencyPairs

Возвращает список доступных для торговли валютных пар и их торговые
характеристики. Партнёр определяется по метаданным `partner-id`, которые SDK
добавляет к каждому запросу, поэтому параметры не нужны.

```go
pairs, err := client.GetCurrencyPairs(ctx)
if err != nil {
    log.Fatalf("ошибка получения валютных пар: %v", err)
}

for _, p := range pairs {
    log.Printf("%s active=%v min_lot=%s min_trade_qty=%s valid_rate_percent=%d nbt_rate=%s",
        p.Pair, p.IsActive, p.MinLot, p.MinTradeQuantity, p.ValidRatePercent, p.NbtRate)
}
```

**Элемент `v1.CurrencyPair`:**

| Поле | Тип | Описание |
|------|-----|----------|
| `Pair` | `string` | Валютная пара, например `"USD/TJS"` |
| `MinLot` | `string` | Минимальный лот (десятичная строка) |
| `MinTradeQuantity` | `string` | Минимальный торгуемый объём (десятичная строка) |
| `ValidRatePercent` | `int32` | Допустимый коридор в % вокруг курса |
| `NbtRate` | `string` | Курс НБТ (десятичная строка) |
| `IsActive` | `bool` | Открыта ли пара для торговли |

Пару с `IsActive = false` торговать нельзя — Core отклонит ордер. `MinLot` и
`MinTradeQuantity` удобно проверять до вызова `SubmitOrder`, а `ValidRatePercent`
задаёт, насколько `LimitRate` может отклоняться от курса, чтобы ордер был принят.

> **Переименование поля.** Раньше это поле называлось `NbtAvgRate`
> (`nbt_avg_rate` в proto). Теперь — `NbtRate` (`nbt_rate`): это курс НБТ, а не
> усреднённое значение. Номер поля в proto (`5`) и тип не изменились, поэтому
> совместимость на уровне протокола сохранена — обновить нужно только код,
> который читал `NbtAvgRate`.

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

## Восстановление после простоя — RecoverTrades

Стрим доставляет только то, что Core ещё держит в очереди. Если партнёр был
недоступен дольше этого окна — долгий простой, рестарт после выходных, потерянное
соединение, — старые сделки по стриму уже не придут, их нужно забрать запросом.
Это и делает `RecoverTrades`.

Каждая сделка проходит через те же точки входа, что и на стриме: сохранение в
`client_trades`, уменьшение `remaining_quantity` родительского ордера и вызов
вашего обработчика. Поэтому восстановленная сделка списывает остаток ордера
ровно один раз и оплачивается ровно один раз — уже имеющиеся сделки отсекаются
вставкой `ON CONFLICT DO NOTHING`.

**Безопасно вызывать при каждом старте и при живом стриме.**

```go
// Догнать пропущенное за вчера и сегодня.
res, err := client.RecoverTrades(ctx, &v1.RecoverParams{
    PartnerId: partnerId,
    DayFrom:   time.Now().In(v1.TimeZone).AddDate(0, 0, -1).Format("2006-01-02"),
    DayTo:     v1.Today(), // необязательно; по умолчанию равен DayFrom
}, handleTrade)
if err != nil {
    log.Printf("recover trades: %v", err)
}
if res != nil && res.Recovered > 0 {
    log.Printf("восстановлено %d сделок из %d, рассчитано %d",
        res.Recovered, res.CoreTrades, res.Settled)
}
```

Поля `RecoverResult`:

| Поле | Описание |
|------|----------|
| `Days` | Дни, которые были запрошены, от старых к новым |
| `CoreTrades` | Сколько сделок вернул Core за всё окно — объём проверки, а не проблемы |
| `Recovered` | Сколько из них отсутствовало локально и было сохранено этим запуском. **Любое ненулевое значение — это данные, которые не доставил стрим** |
| `Settled` | Сколько сделок успешно провёл ваш обработчик за этот запуск |
| `SettleFailed` | Сохранены, но обработчик вернул ошибку. Остаются `settled = FALSE`, их подберёт `RetryUnsettled` |
| `Failed` | Не удалось сохранить вообще — требует разбора (см. ниже) |

**Два отличия от сделки, пришедшей по стриму:**

- Восстановленная сделка не подтверждается ack — ack существует только на
  стриме. Её `client_trades.ack` остаётся `FALSE`, и по этому признаку
  «вытянутую» сделку можно отличить от «принятой». Если Core всё ещё держит её
  неподтверждённой, он доставит её при следующем подключении стрима — эта
  повторная доставка будет no-op.
- Сделка, родительского ордера которой нет в `client_orders`, не может быть
  обогащена (нет `client_id`, счетов, конфигурации комиссии), поэтому она
  попадает в `Failed` и **не сохраняется**. Это реальная несогласованность:
  SDK записывает ордер до того, как отправить его в Core, так что сделки на
  неизвестный ордер быть не должно.

**Ограничения.** За один вызов — не больше 31 дня (каждый день это отдельный
запрос к Core и полный проход по его сделкам). Более широкое окно разбивайте на
части. При ошибке параметров возвращаются `ErrPartnerIDRequired`, `ErrInvalidDay`
или `ErrInvalidRecoverRange`. Ненулевая ошибка означает, что окно обработано не
полностью, но возвращённый `RecoverResult` всё равно описывает всё сделанное до
неё.

> Обработчик ДОЛЖЕН быть идемпотентным — сделка может быть представлена ему
> повторно.

---

## Сверка с Core — Reconcile, ReconcileDiff, ReconcileRepair

`RecoverTrades` догоняет то, про что вы **знаете**, что пропустили. Сверка
отвечает на другой вопрос: не разошлись ли данные незаметно. Она сравнивает
множество ваших сделок за один час с множеством сделок Core за тот же час.

### Что сравнивается

Порядко-независимая XOR-контрольная сумма по идентификаторам сделок:

```sql
bit_xor(hashint8(trade_id) # hashint8(order_id))
```

Обе стороны считают одно и то же выражение — Core по своей таблице расчётов, SDK
по `client_trades`. Равные суммы означают, что множества сделок совпадают.

**Что в сверку не входит:**

- **Флаг `settled`.** В сумму попадает каждая сохранённая сделка, рассчитана она
  или нет. Проведение средств — ваша внутренняя бухгалтерия, Core про неё ничего
  не знает, и учитывать её в сумме значило бы сообщать о расхождении там, где
  данные идентичны. За неоплаченные сделки отвечает `RetryUnsettled`.
- **Суммы и курсы.** В хэш входят только идентификаторы. Расхождение в объёме
  или курсе контрольная сумма не увидит — это находит `ReconcileDiff`.

Контрольная сумма отвечает только на вопрос **«час разошёлся?»**, но никогда —
«какие именно сделки разошлись».

### Окно

Окно — `[Day Hour:00:00, Day Hour+1:00:00)` по `executed_at`, то есть по времени
исполнения в Core, а не по времени записи строки. Границы — стенные часы
**UTC+05:00** с обеих сторон (см. [Часовой пояс](#часовой-пояс--utc0500)).

**Сверяйте завершённый час.** Сверка текущего, ещё идущего часа сравнивает
движущееся множество с движущимся и даст ложные расхождения. Для выбора часа
есть `v1.PreviousHour()`.

### Область видимости — `partner_id` против `sdk_id`

Сверка работает не в тех границах, что обычное чтение сделок, и это важно для
ключа, который торгует за нескольких партнёров.

| Операция | Область |
|----------|---------|
| `GetTrades`, `RecoverTrades` | Сделки указанного `partner_id` — партнёр читает свои книги |
| `Reconcile`, `ReconcileDiff`, `ReconcileRepair` | Всё, что торговал ваш ключ SDK, независимо от того, какому партнёру принадлежит сделка |

Core считает свою половину контрольной суммы по аутентифицированному `sdk_id`,
поэтому локальная половина и разбор расхождения должны покрывать то же самое —
иначе репэйр «чинил» бы час, который никогда не сойдётся. Для ключа, который
обслуживает одного партнёра, оба множества совпадают и разницы нет.

`partner_id` в параметрах сверки нужен, но только для аутентификации вызова —
область он не сужает.

### Три функции

| Функция | Что делает |
|---------|------------|
| `Reconcile` | Только проверяет час и записывает результат. Ничего не меняет |
| `ReconcileDiff` | Разбор расхождения по сделкам. Ничего не меняет |
| `ReconcileRepair` | Проверяет и, если разошлось, чинит то, что чинится автоматически, затем перепроверяет |

Обычный режим работы — таймер с `ReconcileRepair` через несколько минут после
конца часа:

```go
go func() {
    t := time.NewTicker(time.Hour)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            day, hour := v1.PreviousHour() // последний ЗАВЕРШЁННЫЙ час
            params := &v1.ReconcileParams{PartnerId: partnerId, Day: day, Hour: hour}

            res, err := client.ReconcileRepair(ctx, params, handleTrade)
            if err != nil {
                log.Printf("сверка %s ч%02d: %v", day, hour, err)
                continue
            }
            if res.Resolved() {
                continue
            }
            // Автоматически не чинится — нужен человек.
            diff, err := client.ReconcileDiff(ctx, params)
            if err != nil {
                log.Printf("разбор сверки %s ч%02d: %v", day, hour, err)
                continue
            }
            alert("час %s ч%02d разошёлся: нет локально=%d, нет в Core=%d, различаются=%d",
                day, hour, len(diff.MissingLocally), len(diff.MissingRemotely),
                len(diff.Mismatched))
        }
    }
}()
```

Расхождение — **не ошибка**. Оно возвращается через `ReconcileResult.Matched` и
`RepairResult.Resolved()`. Ошибка означает, что саму проверку выполнить не
удалось.

### Результат сверки и таблица `reconciliations`

`Reconcile` записывает результат в таблицу `reconciliations`, ключ — `(dt, hour)`,
запись идемпотентная, так что повторная сверка того же часа безопасна.

| Поле `ReconcileResult` | Описание |
|------------------------|----------|
| `LocalHash` / `RemoteHash` | Контрольные суммы двух сторон |
| `LocalTrades` | Сколько локальных сделок вошло в сумму. `LocalTrades = 0` при `LocalHash = 0` отличает пустой час от настоящего нулевого хэша |
| `Matched` | Суммы совпали |
| `Done` | Хранимый флаг `is_done`: час больше не требует внимания — либо сошёлся, либо партнёр закрыл его вручную после разбора. **Обратно в `false` не возвращается** |

Детали каждой проверки пишутся в `reconciliations.info` (JSONB): обе суммы,
число локальных сделок, границы окна и время проверки — чтобы расхождение можно
было разбирать постфактум.

### Что чинит ReconcileRepair

Автоматически исправляется **одна** ситуация: сделка есть у Core, но так и не
дошла до вашей базы (`TradeDiff.MissingLocally`). Это единственный случай, в
котором реально теряются деньги — расчёт по такой сделке никогда не запускался.
Она сохраняется и оплачивается через те же `persistTrade` + обработчик, что и на
стриме.

Попутно, проходя по сделкам часа, репэйр повторит обработчик для всего, что ещё
не рассчитано. Это бесплатный побочный эффект, но не цель: неоплаченная сделка на
контрольную сумму не влияет, поэтому час, у которого это единственная аномалия,
до `ReconcileRepair` вообще не доходит — его закрывает `RetryUnsettled`.

**Не чинится и требует человека:** локальная сделка, которой нет у Core
(`MissingRemotely`), и сделка, которая есть у обеих сторон, но с разными
значениями (`Mismatched`).

| Поле `RepairResult` | Описание |
|---------------------|----------|
| `Before` | Сверка, решившая, нужен ли ремонт. `Before.Matched = true` — час был чист, счётчики ниже не работали |
| `After` | Перепроверка после ремонта; когда не `nil`, это текущее состояние часа. `nil`, если ремонт не понадобился или ничего не изменил |
| `CoreTrades` | Сколько сделок вернул Core за окно — объём проверки |
| `Recovered` | Сколько из них отсутствовало локально и было сохранено |
| `Settled` / `SettleFailed` | Результаты обработчика на этом запуске |
| `Failed` | Не удалось сохранить (например, нет родительского ордера) — требует разбора |

`Resolved()` — короткий ответ «час чист сейчас»: либо сошёлся сразу, либо сошёлся
после ремонта.

### Разбор расхождения — ReconcileDiff

`ReconcileDiff` забирает список сделок Core за час, читает локальные строки за то
же окно и классифицирует всё, что не совпадает. Ничего не меняет, поэтому его
можно запускать сколько угодно раз.

| Поле `TradeDiff` | Описание |
|------------------|----------|
| `MissingLocally` | Сделки Core, которых нет у вас. Тот самый случай потери денег |
| `MissingRemotely` | Локальные сделки, которых Core не вернул. Обычно артефакт границы окна, а не настоящее расхождение: SDK сохраняет только то, что прислал Core |
| `Mismatched` | Сделки есть у обеих сторон, но значения различаются. `TradeMismatch.Fields` перечисляет разошедшиеся колонки |
| `Unsettled` | Локальные сделки, ещё не проведённые в учёте. **Не расхождение** — обе стороны их имеют и данные совпадают; показываются потому, что разбор часа — удобный момент заметить, что деньги не двинулись |

`Divergent()` отвечает, есть ли настоящее несогласие с Core, и `Unsettled`
намеренно не учитывает.

> Денежные значения сравниваются по величине, а не побайтово: локальные колонки
> `NUMERIC(28,6)` возвращаются как `"10.850000"`, а Core присылает `"10.85"` —
> это одно и то же значение, и `Mismatched` на нём не сработает.

### Если сверять нужно за пределами часа

`GetTrades` возвращает список сделок Core за произвольное окно — только чтение,
ничего не сохраняет, не подтверждает и не рассчитывает:

```go
trades, err := client.GetTrades(ctx, &v1.GetTradesParams{
    PartnerId: partnerId,
    Day:       v1.Today(),
    // DtFrom/DtTo необязательны — по умолчанию весь день.
})
```

Это партнёрская область: возвращаются сделки указанного `partner_id`. Для
разбора расхождения пользуйтесь `ReconcileDiff` — он читает область сверки
(см. [Область видимости](#область-видимости--partner_id-против-sdk_id)).

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

Нулевое значение `Segment` не является сегментом ордера — это значение по
умолчанию для `CounterpartySegment`:

```go
v1.AnyCounterparty // 0 — контрагент из любого сегмента
```

### Тип ордера — `OrderType`

```go
v1.LimitOrder  // 1 — лимитный (по умолчанию, если поле не задано)
v1.MarketOrder // 2 — рыночный: без LimitRate, остаток отменяется
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
| `v1.ErrLimitRateRequired` | Лимитный ордер без `LimitRate` |
| `v1.ErrInvalidOrderType` | `OrderType` не `LimitOrder` и не `MarketOrder` |
| `v1.ErrInvalidCounterpartySegment` | `CounterpartySegment` не `AnyCounterparty` и не `Treasury` |
| `v1.ErrTreasuryCounterpartyOnly` | `CounterpartySegment = Treasury` задан для не-казначейского ордера |

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

// 4. Догнать сделки, пропущенные за время простоя.
_, _ = client.RecoverTrades(ctx, &v1.RecoverParams{
    PartnerId: partnerId,
    DayFrom:   time.Now().In(v1.TimeZone).AddDate(0, 0, -1).Format("2006-01-02"),
    DayTo:     v1.Today(),
}, handleTrade)

// 5. Ежечасная сверка с Core последнего ЗАВЕРШЁННОГО часа.
g.Go(func() error {
    t := time.NewTicker(time.Hour)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-t.C:
            day, hour := v1.PreviousHour()
            res, err := client.ReconcileRepair(ctx,
                &v1.ReconcileParams{PartnerId: partnerId, Day: day, Hour: hour}, handleTrade)
            if err != nil {
                log.Printf("сверка %s ч%02d: %v", day, hour, err)
            } else if !res.Resolved() {
                log.Printf("сверка %s ч%02d разошлась — нужен ReconcileDiff", day, hour)
            }
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
- [ ] Вызывается `RecoverTrades` при старте — догнать пропущенное за простой.
- [ ] Настроена ежечасная сверка `ReconcileRepair` по последнему завершённому
      часу (`v1.PreviousHour()`), с алертом на неразрешённое расхождение.
- [ ] Обработчик сделок `handleTrade` сделан **идемпотентным**.
- [ ] Реализован возврат средств по `OrderEvent.GetReleaseAmount()`.
- [ ] Денежные значения передаются как строки с нужной точностью.
- [ ] Вызывается `client.Close()` при завершении.
