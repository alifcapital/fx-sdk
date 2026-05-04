CREATE TABLE client_orders (
    submitted_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    order_day               DATE NOT NULL DEFAULT CURRENT_DATE,
    ref_id                  BIGSERIAL,
    side                    SMALLINT NOT NULL,
    segment                 SMALLINT NOT NULL,
    status                  SMALLINT NOT NULL DEFAULT 1,
    quantity                NUMERIC(28,6) NOT NULL,
    limit_rate              NUMERIC(28,6) NOT NULL,
    remaining_quantity      NUMERIC(28,6) NOT NULL,
    min_trade_quantity      NUMERIC(28,6),
    allow_partial_fill      BOOLEAN NOT NULL,
    currency_pair           TEXT NOT NULL,
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

CREATE TABLE client_trades (
    executed_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    trading_day           DATE NOT NULL, -- core trade day
    trade_id              BIGINT NOT NULL, -- core trade id
    order_id              BIGINT NOT NULL, -- core order id
    ref_id                BIGINT NOT NULL, -- client_orders ref_id
    filled_quantity       NUMERIC(28,6) NOT NULL,
    execution_rate        NUMERIC(28,6) NOT NULL,
    settlement            NUMERIC(28,6),
    fee                   NUMERIC(28,6),
    client_id             TEXT NOT NULL,
    ack                   BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (trade_id, order_id, trading_day)
) WITH (
    tsdb.hypertable,
    tsdb.partition_column = 'trading_day',
    tsdb.segmentby        = 'client_id',
    tsdb.orderby          = 'trading_day DESC'
);

-- Retention Policy is a set of rules that determines
-- how long data should be kept and when it should be automatically deleted

-- SELECT add_retention_policy('client_trades', INTERVAL '6 months');
-- SELECT add_retention_policy('client_orders', INTERVAL '6 months');


CREATE TABLE reconciliations (
    dt DATE NOT NULL,
    hour SMALLINT NOT NULL,
    is_matched BOOLEAN NOT NULL, -- If hash_check match
    info JSONB,
    is_done BOOLEAN NOT NULL,
    PRIMARY KEY (hour, dt)
) WITH (
    tsdb.hypertable,
    tsdb.partition_column = 'dt',
    tsdb.segmentby        = 'hour',
    tsdb.orderby          = 'dt DESC'
);
-- after test maybe not needed count and sum ?
SELECT
    bit_xor(hashint8(trade_id) # hashint8(order_id)) as hash_check
FROM client_trades
WHERE trading_day = '2026-04-30'
  AND executed_at >= '2026-04-30 10:00:00'
  AND executed_at <  '2026-04-30 11:00:00';
