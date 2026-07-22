package spend

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/the-algovn/radio-service/internal/db"
)

// PGLedger stores ledger lines in Postgres via sqlc.
type PGLedger struct{ pool *pgxpool.Pool }

func NewPGLedger(pool *pgxpool.Pool) *PGLedger { return &PGLedger{pool: pool} }

func (l *PGLedger) Append(ctx context.Context, line Line) error {
	return db.New(l.pool).InsertLedgerLine(ctx, db.InsertLedgerLineParams{
		Ts: line.TS, Kind: line.Kind, Provider: line.Provider, Label: line.Label,
		Chars: int32(line.Chars), InTokens: int32(line.InTokens),
		OutTokens: int32(line.OutTokens), CostUsd: line.CostUSD,
	})
}

func (l *PGLedger) All(ctx context.Context) ([]Line, error) {
	rows, err := db.New(l.pool).ListLedgerLines(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Line, 0, len(rows))
	for _, r := range rows {
		out = append(out, Line{
			TS: r.Ts, Kind: r.Kind, Provider: r.Provider, Label: r.Label,
			Chars: int(r.Chars), InTokens: int(r.InTokens),
			OutTokens: int(r.OutTokens), CostUSD: r.CostUsd,
		})
	}
	return out, nil
}

// TotalCost is the SUM(cost_usd) primitive the Spec-3 budget cap reuses.
func (l *PGLedger) TotalCost(ctx context.Context) (float64, error) {
	return db.New(l.pool).SumLedgerCost(ctx)
}

// SpentSince sums cost at or after since — the programmer's daily budget
// gate (station day boundaries come from the caller).
func (l *PGLedger) SpentSince(ctx context.Context, since time.Time) (float64, error) {
	return db.New(l.pool).SumLedgerCostSince(ctx, since)
}
