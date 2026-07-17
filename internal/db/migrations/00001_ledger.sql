-- +goose Up
CREATE TABLE ledger_line (
    id         BIGSERIAL PRIMARY KEY,
    ts         TIMESTAMPTZ NOT NULL,
    kind       TEXT NOT NULL,
    provider   TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    chars      INT  NOT NULL DEFAULT 0,
    in_tokens  INT  NOT NULL DEFAULT 0,
    out_tokens INT  NOT NULL DEFAULT 0,
    cost_usd   DOUBLE PRECISION NOT NULL
);
CREATE INDEX ledger_line_ts_idx ON ledger_line (ts);

-- +goose Down
DROP TABLE IF EXISTS ledger_line;
