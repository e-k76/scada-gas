package audit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/scada-gas/deploy"
)

// PGStore stores the audit log in PostgreSQL / TimescaleDB. It connects lazily
// and creates its table (deploy/control_init.sql) on first use.
type PGStore struct {
	pool *pgxpool.Pool

	mu    sync.Mutex
	ready bool
}

// NewPGStore prepares a connection pool for dsn without dialling the server.
func NewPGStore(ctx context.Context, dsn string) (*PGStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("audit: parse dsn: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("audit: pool: %w", err)
	}
	return &PGStore{pool: pool}, nil
}

func (s *PGStore) Close()                         { s.pool.Close() }
func (s *PGStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *PGStore) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	if _, err := s.pool.Exec(ctx, deploy.ControlSQL); err != nil {
		return fmt.Errorf("audit: create schema: %w", err)
	}
	s.ready = true
	return nil
}

func (s *PGStore) Begin(ctx context.Context, e Entry) (int64, error) {
	if err := s.ensure(ctx); err != nil {
		return 0, err
	}
	e = Sanitize(e)
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	var id int64
	err := s.pool.QueryRow(ctx, `INSERT INTO command_audit_log
		(time, command_id, operator, role, remote_addr, station_id, tag_id, action, setpoint_value, status, success, message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) RETURNING id`,
		e.Time, e.CommandID, e.Operator, e.Role, e.RemoteAddr, e.StationID, e.TagID, e.Action, e.Setpoint,
		string(e.Status), e.Success, e.Message).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("audit: insert: %w", err)
	}
	return id, nil
}

func (s *PGStore) Complete(ctx context.Context, id int64, r Result) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE command_audit_log
		SET status = $2, success = $3, message = $4, executed_at = $5, latency_ms = $6
		WHERE id = $1 AND status = 'PENDING'`,
		id, string(r.Status), r.Success, clip(r.Message, 1000), r.ExecutedAt, r.LatencyMs)
	if err != nil {
		return fmt.Errorf("audit: complete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotPending
	}
	return nil
}

func (s *PGStore) Recent(ctx context.Context, tagID string, limit int) ([]Entry, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, time, command_id, operator, role, remote_addr, station_id, tag_id, action,
			setpoint_value, status, success, message, executed_at, latency_ms
		FROM command_audit_log WHERE ($1 = '' OR tag_id = $1) ORDER BY time DESC, id DESC LIMIT $2`, tagID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		var st string
		var lat *int32
		if err := rows.Scan(&e.ID, &e.Time, &e.CommandID, &e.Operator, &e.Role, &e.RemoteAddr, &e.StationID, &e.TagID, &e.Action,
			&e.Setpoint, &st, &e.Success, &e.Message, &e.ExecutedAt, &lat); err != nil {
			return nil, err
		}
		e.Status = Status(st)
		if lat != nil {
			v := int64(*lat)
			e.LatencyMs = &v
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return out, nil
}
