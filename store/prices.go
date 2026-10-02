package store

import (
	"context"
	"strings"
)

// ModelPrice is one model's per-million-token rates, as published by a
// pricing source (currently models.dev) or fitted by `prices derive`.
//
// CacheWrite is the 5-minute-TTL cache write rate. CacheWrite1h is the
// 1-hour-TTL rate; sources that publish only one cache-write figure quote
// the 5m one, so the 1h rate is derived (2x base input) at fetch time —
// see fetchModelsDevPrices.
//
// EffectiveFrom/EffectiveTo give the row a validity window in ms epoch, so
// a (provider, model) can carry more than one rate over time — a derived
// rate that changed mid-window (see prices_derive.go) needs one row per
// segment, not one row that's silently wrong for part of the window. 0
// means unbounded on that side: EffectiveFrom==0 is "since forever",
// EffectiveTo==0 is "still in effect". Every models.dev-synced row, and
// any single-segment derive, leaves both at 0 — a fully open window, i.e.
// the same rate at every timestamp, which is the only kind of row that
// existed before this field did.
type ModelPrice struct {
	Provider      string
	Model         string
	Name          string
	Input         float64
	Output        float64
	CacheRead     float64
	CacheWrite    float64
	CacheWrite1h  float64
	Source        string
	UpdatedAt     int64 // ms epoch of the fetch/fit that produced this row
	EffectiveFrom int64
	EffectiveTo   int64
}

// UpsertModelPrices writes prices, replacing any existing row for the same
// (provider, model, effective_from) window. One transaction for the whole
// batch.
func (s *Store) UpsertModelPrices(ctx context.Context, prices []ModelPrice) error {
	if len(prices) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO model_price
		  (provider, model, name, input, output, cache_read, cache_write, cache_write_1h,
		   source, updated_at, effective_from, effective_to)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, model, effective_from) DO UPDATE SET
		  name           = excluded.name,
		  input          = excluded.input,
		  output         = excluded.output,
		  cache_read     = excluded.cache_read,
		  cache_write    = excluded.cache_write,
		  cache_write_1h = excluded.cache_write_1h,
		  source         = excluded.source,
		  updated_at     = excluded.updated_at,
		  effective_to   = excluded.effective_to
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range prices {
		if _, err := stmt.ExecContext(ctx, p.Provider, p.Model, p.Name,
			p.Input, p.Output, p.CacheRead, p.CacheWrite, p.CacheWrite1h,
			p.Source, p.UpdatedAt, p.EffectiveFrom, p.EffectiveTo); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteModelPrices removes every row for the given (provider, model)
// pairs, but only if their stored source matches — a defensive filter so a
// caller retracting its own rows can never accidentally delete a row it
// didn't write, even if the (provider, model) key were ever reused by
// another source. The WHERE clause deliberately doesn't reference
// effective_from, so this deletes every window a (provider, model) has,
// not just one — the caller (prices_derive.go) always deletes a model's
// entire window set before reinserting whatever segments qualified on the
// current run, since a re-run's segment boundaries rarely line up exactly
// with the previous run's.
func (s *Store) DeleteModelPrices(ctx context.Context, source string, pairs [][2]string) error {
	if len(pairs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `DELETE FROM model_price WHERE source = ? AND provider = ? AND model = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range pairs {
		if _, err := stmt.ExecContext(ctx, source, p[0], p[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteModelPricesByProviders removes every row for the given (source,
// provider) combination, regardless of model. It exists for one-time
// migrations off an old provider-namespacing scheme, where the exact set of
// models under the old scheme isn't known up front — see prices derive's
// switch from provider="pi"/"opencode" to "derived:pi"/"derived:opencode".
func (s *Store) DeleteModelPricesByProviders(ctx context.Context, source string, providers []string) (int64, error) {
	if len(providers) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(providers))
	args := make([]any, 0, len(providers)+1)
	args = append(args, source)
	for i, p := range providers {
		placeholders[i] = "?"
		args = append(args, p)
	}
	query := `DELETE FROM model_price WHERE source = ? AND provider IN (` + strings.Join(placeholders, ",") + `)`
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetModelPrices returns stored prices for one provider, or for every
// provider when provider is empty. Ordered by provider, then model, then
// effective_from — a model with more than one window (see ModelPrice)
// comes back oldest-window-first, which is the order resolveAgentWindow
// and resolveClaudeWindow expect to scan in.
func (s *Store) GetModelPrices(ctx context.Context, provider string) ([]ModelPrice, error) {
	query := `
		SELECT provider, model, COALESCE(name, ''), input, output,
		       cache_read, cache_write, cache_write_1h, source, updated_at,
		       effective_from, effective_to
		FROM model_price
	`
	var args []any
	if provider != "" {
		query += ` WHERE provider = ?`
		args = append(args, provider)
	}
	query += ` ORDER BY provider ASC, model ASC, effective_from ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModelPrice
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.Provider, &p.Model, &p.Name, &p.Input, &p.Output,
			&p.CacheRead, &p.CacheWrite, &p.CacheWrite1h, &p.Source, &p.UpdatedAt,
			&p.EffectiveFrom, &p.EffectiveTo); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
