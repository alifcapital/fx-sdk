-- The FX Core operates in UTC+05:00 (Tajikistan time, no DST), so every date
-- and hour in these tables is the UTC+05:00 calendar date and wall clock, never
-- the session's. The SDK anchors it explicitly with
-- AT TIME ZONE INTERVAL '+05:00' rather than relying on the session TimeZone,
-- so nothing here depends on how the pool is configured.
--
-- Use the INTERVAL form, not the string form: PostgreSQL reads
-- AT TIME ZONE '+05' with the inverted POSIX sign convention, which puts a
-- 10:00 wall clock at 15:00Z instead of 05:00Z.
--
-- Timestamp columns stay TIMESTAMPTZ and so store an unambiguous instant; the
-- anchoring only decides which calendar day and which hour an instant belongs
-- to.
CREATE TABLE client_orders (
    submitted_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- the UTC+05:00 calendar day, not CURRENT_DATE: around midnight a session
    -- running in UTC would file the order under the previous day, and the Core
    -- joins a trade back to its order through this value.
    order_day               DATE NOT NULL DEFAULT (NOW() AT TIME ZONE INTERVAL '+05:00')::date,
    ref_id                  BIGSERIAL,
    order_id                BIGINT,
    side                    SMALLINT NOT NULL,
    segment                 SMALLINT NOT NULL,
    status                  SMALLINT NOT NULL DEFAULT 1,
    order_type              SMALLINT NOT NULL DEFAULT 1,  -- 1 = limit, 2 = market
    counterparty_segment    SMALLINT NOT NULL DEFAULT 0,  -- 0 = any counterparty, 3 = treasury only
    quantity                NUMERIC(28,6) NOT NULL,
    limit_rate              NUMERIC(28,6),                -- NULL for market orders (priced by the book)
    remaining_quantity      NUMERIC(28,6) NOT NULL,
    min_trade_quantity      NUMERIC(28,6),                -- NULL = the currency pair's minimum
    currency_pair           TEXT NOT NULL,
    partner_id              TEXT NOT NULL,
    client_id               TEXT NOT NULL,           -- client's id
    client_inn              TEXT NOT NULL,           -- client's INN (tax identifier)
    cause                   TEXT,
    account                 JSONB,
    fee                     JSONB,
    PRIMARY KEY (ref_id, order_day)
) WITH (
    tsdb.hypertable,
    tsdb.partition_column = 'order_day',
    tsdb.segmentby        = 'client_id',
    tsdb.orderby          = 'order_day DESC'
);

-- Migration for databases created before market orders and counterparty
-- restrictions existed. Safe to re-run.
-- ALTER TABLE client_orders ADD COLUMN IF NOT EXISTS order_type SMALLINT NOT NULL DEFAULT 1;
-- ALTER TABLE client_orders ADD COLUMN IF NOT EXISTS counterparty_segment SMALLINT NOT NULL DEFAULT 0;
-- ALTER TABLE client_orders ALTER COLUMN limit_rate DROP NOT NULL;

CREATE TABLE client_trades (
    -- The Core's execution time for this fill, written from the trade event --
    -- NOT the moment the row was stored, so a trade stored late still sits in
    -- the hour it was executed in, the same instant as the Core's trade_date.
    -- The default only applies if the Core sent a value that could not be
    -- parsed.
    executed_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    trading_day           DATE NOT NULL, -- core trade day
    trade_id              BIGINT NOT NULL, -- core trade id
    order_id              BIGINT NOT NULL, -- core order id
    ref_id                BIGINT NOT NULL, -- client_orders ref_id
    side                  SMALLINT NOT NULL,
    trade_type            SMALLINT NOT NULL DEFAULT 0,      -- 1 same partner, 2 same partner + Treasury, 3 cross-partner; 0 not reported
    settle_attempts       SMALLINT NOT NULL DEFAULT 0,      -- number of settlement handler attempts
    filled_quantity       NUMERIC(28,6) NOT NULL,
    execution_rate        NUMERIC(28,6) NOT NULL,
    settlement            NUMERIC(28,6),
    fee                   NUMERIC(28,6),
    partner_id            TEXT NOT NULL,
    client_id             TEXT NOT NULL,
    ack                   BOOLEAN NOT NULL DEFAULT FALSE,   -- trade was acked to the Core (received & stored)
    settled               BOOLEAN NOT NULL DEFAULT FALSE,   -- partner-side settlement (debit/credit) succeeded
    settle_error          TEXT,                             -- last settlement error, NULL once settled
    PRIMARY KEY (trade_id, order_id, trading_day)
) WITH (
    tsdb.hypertable,
    tsdb.partition_column = 'trading_day',
    tsdb.segmentby        = 'client_id',
    tsdb.orderby          = 'trading_day DESC'
);

-- Backstop lookup for RetryUnsettled: trades stored but not yet settled.
CREATE INDEX client_trades_unsettled ON client_trades (trading_day) WHERE NOT settled;

-- Retention Policy is a set of rules that determines
-- how long data should be kept and when it should be automatically deleted

-- SELECT add_retention_policy('client_trades', INTERVAL '6 months');
-- SELECT add_retention_policy('client_orders', INTERVAL '6 months');


-- The reconciliations table of earlier versions is no longer used: hourly
-- trade reconciliation was replaced by ReconcileOrders. An existing database
-- may drop it:
--
-- DROP TABLE IF EXISTS reconciliations;

-- Migration for a database created before the offset was pinned. Existing rows
-- are left alone: order_day was written from the session's CURRENT_DATE, and
-- only a partner whose session was not already +05 has rows to review.
--
-- ALTER TABLE client_orders
--   ALTER COLUMN order_day SET DEFAULT (NOW() AT TIME ZONE INTERVAL '+05:00')::date;
