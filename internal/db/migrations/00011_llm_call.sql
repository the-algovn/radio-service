-- +goose Up
-- LLM call audit (spec 2026-07-27): full prompt/output/metadata per call.
-- Rolling 30-day retention, pruned on write in PGStore.Record.
CREATE TABLE llm_call (
  id            BIGSERIAL PRIMARY KEY,
  ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
  label         TEXT NOT NULL DEFAULT '',
  model         TEXT NOT NULL,
  provider      TEXT NOT NULL,
  system_prompt TEXT NOT NULL DEFAULT '',
  user_prompt   TEXT NOT NULL DEFAULT '',
  output        TEXT NOT NULL DEFAULT '',
  in_tokens     INT  NOT NULL DEFAULT 0,
  out_tokens    INT  NOT NULL DEFAULT 0,
  cost_usd      DOUBLE PRECISION NOT NULL DEFAULT 0,
  latency_ms    INT  NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT '',
  fake          BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX llm_call_ts_idx ON llm_call (ts DESC);
CREATE INDEX llm_call_label_ts_idx ON llm_call (label, ts DESC);

-- +goose Down
DROP TABLE llm_call;
