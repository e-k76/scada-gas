package historian

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TrendPoint is one downsampled bucket of an analog tag.
type TrendPoint struct {
	Time    time.Time
	Avg     float64
	Min     float64
	Max     float64
	Count   int64
	BadQual bool // at least one stored sample in the bucket had non-GOOD quality
}

// TrendSeries is the downsampled history of one tag.
type TrendSeries struct {
	TagID  string
	Points []TrendPoint
}

// Reader runs read-only history queries. It connects lazily, so it can be
// created while the database is still starting.
type Reader struct {
	pool *pgxpool.Pool

	tsOnce sync.Once
	hasTS  bool
}

// NewReader prepares a connection pool for dsn without dialling the server.
func NewReader(ctx context.Context, dsn string) (*Reader, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("historian: parse dsn: %w", err)
	}
	cfg.MaxConns = 4
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("historian: pool: %w", err)
	}
	return &Reader{pool: pool}, nil
}

// Close releases all connections.
func (r *Reader) Close() { r.pool.Close() }

// Ping checks that the database is reachable.
func (r *Reader) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

// timescaleAvailable reports (once) whether the timescaledb extension is
// installed, so time_bucket() can be used; otherwise plain SQL bucketing is used.
func (r *Reader) timescaleAvailable(ctx context.Context) bool {
	r.tsOnce.Do(func() {
		var ok bool
		err := r.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&ok)
		r.hasTS = err == nil && ok
	})
	return r.hasTS
}

const trendSelect = `
SELECT tag_id, %s AS bucket, avg(value), min(value), max(value), count(*), bool_or(quality <> 1)
FROM analog_telemetry
WHERE tag_id = ANY($1) AND time >= $2 AND time < $3
GROUP BY tag_id, bucket
ORDER BY tag_id, bucket`

const (
	timescaleBucket = `time_bucket(make_interval(secs => $4::double precision), time)`
	plainBucket     = `to_timestamp(floor(extract(epoch FROM time) / $4::double precision) * $4::double precision)`
)

// Trend returns avg/min/max per bucket for each requested tag in [start,end).
// Tags without data in the range yield an empty series.
func (r *Reader) Trend(ctx context.Context, tags []string, start, end time.Time, bucket time.Duration) ([]TrendSeries, error) {
	if len(tags) == 0 {
		return nil, errors.New("historian: no tags requested")
	}
	if !end.After(start) {
		return nil, errors.New("historian: end must be after start")
	}
	if bucket < time.Second {
		bucket = time.Second
	}
	expr := plainBucket
	if r.timescaleAvailable(ctx) {
		expr = timescaleBucket
	}
	rows, err := r.pool.Query(ctx, fmt.Sprintf(trendSelect, expr), tags, start.UTC(), end.UTC(), bucket.Seconds())
	if err != nil {
		return nil, fmt.Errorf("historian: trend query: %w", err)
	}
	defer rows.Close()

	byTag := make(map[string]*TrendSeries, len(tags))
	out := make([]TrendSeries, len(tags))
	for i, t := range tags {
		out[i].TagID = t
		byTag[t] = &out[i]
	}
	for rows.Next() {
		var tag string
		var p TrendPoint
		if err := rows.Scan(&tag, &p.Time, &p.Avg, &p.Min, &p.Max, &p.Count, &p.BadQual); err != nil {
			return nil, fmt.Errorf("historian: scan trend row: %w", err)
		}
		if s := byTag[tag]; s != nil {
			s.Points = append(s.Points, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("historian: trend rows: %w", err)
	}
	return out, nil
}
