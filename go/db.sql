CREATE TABLE orders (
    submitted_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    id                      UUID PRIMARY KEY DEFAULT uuidv7(),
    order_id                BIGINT,                  -- remote order ID from the Core
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
    fee                     JSONB
) WITH (
    tsdb.hypertable,
    tsdb.partition_column = 'id',
    tsdb.segmentby        = 'client_id',
    tsdb.orderby          = 'id DESC'
);

-- Index for duplicate order detection (2-minute window check)
CREATE INDEX IF NOT EXISTS idx_orders_duplicate_check ON orders (client_id, side, limit_rate, quantity, submitted_at DESC);
