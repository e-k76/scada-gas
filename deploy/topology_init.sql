-- Station topology storage for the Engineering Studio (plain PostgreSQL,
-- also valid on TimescaleDB). Idempotent: safe to run on every start; the
-- gateway applies it automatically, and docker-compose runs it on first boot.

-- Current (latest saved) drawing of every station.
CREATE TABLE IF NOT EXISTS station_topology (
    station_id       TEXT        PRIMARY KEY,
    name             TEXT        NOT NULL,
    version          BIGINT      NOT NULL CHECK (version >= 1),
    document         JSONB       NOT NULL,           -- model.StationTopology
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by       TEXT        NOT NULL DEFAULT '',
    deployed_version BIGINT      NOT NULL DEFAULT 0, -- 0 = never deployed
    deployed_at      TIMESTAMPTZ,
    deployed_by      TEXT        NOT NULL DEFAULT ''
);

-- Every saved version (the gateway keeps the latest 100 plus the deployed one),
-- so the exact drawing that runs in production can always be recovered.
CREATE TABLE IF NOT EXISTS station_topology_history (
    station_id TEXT        NOT NULL,
    version    BIGINT      NOT NULL,
    document   JSONB       NOT NULL,
    saved_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    saved_by   TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (station_id, version)
);

CREATE INDEX IF NOT EXISTS station_topology_history_saved_idx
    ON station_topology_history (station_id, saved_at DESC);
