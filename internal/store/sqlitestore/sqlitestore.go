// Package sqlitestore is a durable store.Repository backed by SQLite
// (modernc.org/sqlite — pure Go, cgo-free). Bars are rows so the engine slices.
package sqlitestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/mwasilew2/alpaca-playground/internal/marketdata"
	"github.com/mwasilew2/alpaca-playground/internal/store"

	_ "modernc.org/sqlite"
)

type repo struct{ db *sql.DB }

// Open opens (creating if needed) a SQLite-backed Repository at path.
func Open(path string) (store.Repository, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("sqlitestore: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // modernc + single-writer: serialize to avoid lock churn
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlitestore: %s: %w", pragma, err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &repo{db: db}, nil
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS bars (
			symbol TEXT NOT NULL, timeframe TEXT NOT NULL, t INTEGER NOT NULL,
			o REAL, h REAL, l REAL, c REAL, v INTEGER, n INTEGER, vw REAL,
			PRIMARY KEY (symbol, timeframe, t)
		) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS fetched_ranges (
			symbol TEXT NOT NULL, timeframe TEXT NOT NULL,
			from_ns INTEGER NOT NULL, to_ns INTEGER NOT NULL, fetched_at_ns INTEGER NOT NULL,
			PRIMARY KEY (symbol, timeframe, from_ns)
		)`,
		`CREATE TABLE IF NOT EXISTS schema_meta (version INTEGER NOT NULL)`,
		`INSERT INTO schema_meta (version) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM schema_meta)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("sqlitestore: migrate: %w", err)
		}
	}
	return nil
}

func (r *repo) Bars(ctx context.Context, symbol, timeframe string, start, end time.Time) ([]marketdata.Bar, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT t,o,h,l,c,v,n,vw FROM bars
		 WHERE symbol=? AND timeframe=? AND t BETWEEN ? AND ? ORDER BY t`,
		symbol, timeframe, start.UnixNano(), end.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: bars query: %w", err)
	}
	defer rows.Close()
	var out []marketdata.Bar
	for rows.Next() {
		var tn int64
		var b marketdata.Bar
		if err := rows.Scan(&tn, &b.O, &b.H, &b.L, &b.C, &b.V, &b.N, &b.VW); err != nil {
			return nil, fmt.Errorf("sqlitestore: bars scan: %w", err)
		}
		b.T = time.Unix(0, tn).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *repo) Intervals(ctx context.Context, symbol, timeframe string) ([]store.Interval, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT from_ns,to_ns,fetched_at_ns FROM fetched_ranges WHERE symbol=? AND timeframe=?`,
		symbol, timeframe)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: intervals query: %w", err)
	}
	defer rows.Close()
	var out []store.Interval
	for rows.Next() {
		var f, t, fa int64
		if err := rows.Scan(&f, &t, &fa); err != nil {
			return nil, fmt.Errorf("sqlitestore: intervals scan: %w", err)
		}
		out = append(out, store.Interval{
			From: time.Unix(0, f).UTC(), To: time.Unix(0, t).UTC(), FetchedAt: time.Unix(0, fa).UTC(),
		})
	}
	return out, rows.Err()
}

func (r *repo) PutBars(ctx context.Context, symbol, timeframe string, bars []marketdata.Bar) error {
	if len(bars) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: putbars begin: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO bars (symbol,timeframe,t,o,h,l,c,v,n,vw) VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(symbol,timeframe,t) DO UPDATE SET
		   o=excluded.o,h=excluded.h,l=excluded.l,c=excluded.c,v=excluded.v,n=excluded.n,vw=excluded.vw`)
	if err != nil {
		return fmt.Errorf("sqlitestore: putbars prepare: %w", err)
	}
	defer stmt.Close()
	for _, b := range bars {
		if _, err := stmt.ExecContext(ctx, symbol, timeframe, b.T.UnixNano(), b.O, b.H, b.L, b.C, b.V, b.N, b.VW); err != nil {
			return fmt.Errorf("sqlitestore: putbars exec: %w", err)
		}
	}
	return tx.Commit()
}

func (r *repo) PutIntervals(ctx context.Context, symbol, timeframe string, intervals []store.Interval) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: putintervals begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM fetched_ranges WHERE symbol=? AND timeframe=?`, symbol, timeframe); err != nil {
		return fmt.Errorf("sqlitestore: putintervals delete: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO fetched_ranges (symbol,timeframe,from_ns,to_ns,fetched_at_ns) VALUES (?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("sqlitestore: putintervals prepare: %w", err)
	}
	defer stmt.Close()
	for _, iv := range intervals {
		if _, err := stmt.ExecContext(ctx, symbol, timeframe, iv.From.UnixNano(), iv.To.UnixNano(), iv.FetchedAt.UnixNano()); err != nil {
			return fmt.Errorf("sqlitestore: putintervals exec: %w", err)
		}
	}
	return tx.Commit()
}

func (r *repo) Close() error { return r.db.Close() }
