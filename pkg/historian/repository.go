package historian

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AnalogRow is one row of analog_telemetry.
type AnalogRow struct {
	TimeMs  int64
	TagID   string
	Value   float64
	Quality int32
}

// DigitalRow is one row of digital_telemetry.
type DigitalRow struct {
	TimeMs  int64
	TagID   string
	State   bool
	Quality int32
}

// AlarmRow is one row of alarm_history.
type AlarmRow struct {
	TimeMs       int64
	AlarmID      string
	TagID        string
	Severity     int32
	State        int32
	Message      string
	TriggerValue float64
	AckByUser    string
}

// Store is the persistence interface used by the Ingester.
type Store interface {
	InsertAnalog(ctx context.Context, rows []AnalogRow) error
	InsertDigital(ctx context.Context, rows []DigitalRow) error
	InsertAlarms(ctx context.Context, rows []AlarmRow) error
	Close()
}

// Repository is the PostgreSQL / TimescaleDB implementation of Store. It uses
// the COPY protocol, which is the fastest way to bulk-load rows with pgx.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository connects to the database and verifies the connection.
func NewRepository(ctx context.Context, dsn string) (*Repository, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("historian: parse dsn: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("historian: connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("historian: ping: %w", err)
	}
	return &Repository{pool: pool}, nil
}

// Close releases all connections.
func (r *Repository) Close() { r.pool.Close() }

func ts(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// InsertAnalog bulk-inserts analog rows in a single COPY.
func (r *Repository) InsertAnalog(ctx context.Context, rows []AnalogRow) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := r.pool.CopyFrom(ctx,
		pgx.Identifier{"analog_telemetry"},
		[]string{"time", "tag_id", "value", "quality"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			x := rows[i]
			return []any{ts(x.TimeMs), x.TagID, x.Value, x.Quality}, nil
		}))
	if err != nil {
		return fmt.Errorf("historian: copy analog_telemetry: %w", err)
	}
	return nil
}

// InsertDigital bulk-inserts digital rows in a single COPY.
func (r *Repository) InsertDigital(ctx context.Context, rows []DigitalRow) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := r.pool.CopyFrom(ctx,
		pgx.Identifier{"digital_telemetry"},
		[]string{"time", "tag_id", "state", "quality"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			x := rows[i]
			return []any{ts(x.TimeMs), x.TagID, x.State, x.Quality}, nil
		}))
	if err != nil {
		return fmt.Errorf("historian: copy digital_telemetry: %w", err)
	}
	return nil
}

// InsertAlarms bulk-inserts alarm history rows in a single COPY.
func (r *Repository) InsertAlarms(ctx context.Context, rows []AlarmRow) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := r.pool.CopyFrom(ctx,
		pgx.Identifier{"alarm_history"},
		[]string{"time", "alarm_id", "tag_id", "severity", "state", "message", "trigger_value", "ack_by_user"},
		pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			x := rows[i]
			return []any{ts(x.TimeMs), x.AlarmID, x.TagID, x.Severity, x.State, x.Message, x.TriggerValue, x.AckByUser}, nil
		}))
	if err != nil {
		return fmt.Errorf("historian: copy alarm_history: %w", err)
	}
	return nil
}
