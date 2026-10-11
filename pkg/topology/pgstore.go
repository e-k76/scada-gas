package topology

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/scada-gas/deploy"
	"github.com/example/scada-gas/pkg/model"
)

const keepHistory = 100

// PGStore stores topologies in PostgreSQL / TimescaleDB. It connects lazily and
// creates its tables (deploy/topology_init.sql) on first use, so the gateway
// can start before the database does and works on pre-existing databases.
type PGStore struct {
	pool *pgxpool.Pool

	mu    sync.Mutex
	ready bool
}

// NewPGStore prepares a connection pool for dsn without dialling the server.
func NewPGStore(ctx context.Context, dsn string) (*PGStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("topology: parse dsn: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("topology: pool: %w", err)
	}
	return &PGStore{pool: pool}, nil
}

// Close releases all connections.
func (s *PGStore) Close() { s.pool.Close() }

// Ping checks that the database is reachable.
func (s *PGStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ensure creates the schema once.
func (s *PGStore) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ready {
		return nil
	}
	if _, err := s.pool.Exec(ctx, deploy.TopologySQL); err != nil {
		return fmt.Errorf("topology: create schema: %w", err)
	}
	s.ready = true
	return nil
}

const recordColumns = `station_id, name, version, document, updated_at, updated_by, deployed_version, deployed_at, deployed_by`

func scanRecord(row pgx.Row) (*Record, error) {
	var r Record
	var doc []byte
	if err := row.Scan(&r.StationID, &r.Name, &r.Version, &doc, &r.UpdatedAt, &r.UpdatedBy, &r.DeployedVersion, &r.DeployedAt, &r.DeployedBy); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var t model.StationTopology
	if err := json.Unmarshal(doc, &t); err != nil {
		return nil, fmt.Errorf("topology: stored document of %s is corrupt: %w", r.StationID, err)
	}
	t.Normalize()
	r.Document = &t
	return &r, nil
}

func (s *PGStore) Get(ctx context.Context, id string) (*Record, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	return scanRecord(s.pool.QueryRow(ctx, `SELECT `+recordColumns+` FROM station_topology WHERE station_id = $1`, id))
}

func (s *PGStore) GetVersion(ctx context.Context, id string, version int64) (*model.StationTopology, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	var doc []byte
	err := s.pool.QueryRow(ctx, `SELECT document FROM station_topology_history WHERE station_id = $1 AND version = $2`, id, version).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var t model.StationTopology
	if err := json.Unmarshal(doc, &t); err != nil {
		return nil, fmt.Errorf("topology: stored document of %s v%d is corrupt: %w", id, version, err)
	}
	t.Normalize()
	return &t, nil
}

func (s *PGStore) List(ctx context.Context) ([]Summary, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT station_id, name, version, updated_at, updated_by, deployed_version, deployed_at, deployed_by
		FROM station_topology ORDER BY station_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var x Summary
		if err := rows.Scan(&x.StationID, &x.Name, &x.Version, &x.UpdatedAt, &x.UpdatedBy, &x.DeployedVersion, &x.DeployedAt, &x.DeployedBy); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *PGStore) Save(ctx context.Context, doc *model.StationTopology, by string, expected int64) (*Record, error) {
	if err := s.ensure(ctx); err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cur int64
	err = tx.QueryRow(ctx, `SELECT version FROM station_topology WHERE station_id = $1 FOR UPDATE`, doc.StationID).Scan(&cur)
	exists := true
	if errors.Is(err, pgx.ErrNoRows) {
		exists, cur = false, 0
	} else if err != nil {
		return nil, err
	}
	if expected != cur {
		return nil, &ConflictError{Current: cur}
	}

	next := cur + 1
	now := time.Now().UTC()
	d := cloneDoc(doc)
	d.Version, d.UpdatedAtMs, d.UpdatedBy = next, now.UnixMilli(), by
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if exists {
		_, err = tx.Exec(ctx, `UPDATE station_topology SET name = $2, version = $3, document = $4::jsonb, updated_at = $5, updated_by = $6
			WHERE station_id = $1`, d.StationID, d.Name, next, raw, now, by)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO station_topology (station_id, name, version, document, updated_at, updated_by)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6)`, d.StationID, d.Name, next, raw, now, by)
	}
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" { // two creators raced
			return nil, &ConflictError{Current: 1}
		}
		return nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO station_topology_history (station_id, version, document, saved_at, saved_by)
		VALUES ($1, $2, $3::jsonb, $4, $5)`, d.StationID, next, raw, now, by); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM station_topology_history h
		WHERE h.station_id = $1 AND h.version < $2
		  AND h.version <> (SELECT deployed_version FROM station_topology WHERE station_id = $1)`, d.StationID, next-keepHistory); err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.Get(ctx, d.StationID)
}

func (s *PGStore) MarkDeployed(ctx context.Context, id string, version int64, by string) error {
	if err := s.ensure(ctx); err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE station_topology SET deployed_version = $2, deployed_at = now(), deployed_by = $3
		WHERE station_id = $1 AND version >= $2 AND $2 >= 1`, id, version, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
