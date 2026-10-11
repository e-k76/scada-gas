-- SCADA historian schema for TimescaleDB (PostgreSQL 14+).
-- Executed automatically by the timescaledb container on first start
-- (mounted into /docker-entrypoint-initdb.d).

CREATE EXTENSION IF NOT EXISTS timescaledb;

-- ---------------------------------------------------------------------------
-- Analog telemetry (deadband-filtered engineering values)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS analog_telemetry (
    time    TIMESTAMPTZ      NOT NULL,
    tag_id  TEXT             NOT NULL,
    value   DOUBLE PRECISION NOT NULL,
    quality INTEGER          NOT NULL   -- scada.v1.DataQuality
);

SELECT create_hypertable('analog_telemetry', 'time',
                         chunk_time_interval => INTERVAL '1 day',
                         if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS analog_telemetry_tag_time_idx
    ON analog_telemetry (tag_id, time DESC);

ALTER TABLE analog_telemetry SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'tag_id',
    timescaledb.compress_orderby   = 'time DESC'
);
SELECT add_compression_policy('analog_telemetry', INTERVAL '7 days', if_not_exists => TRUE);

-- ---------------------------------------------------------------------------
-- Digital telemetry (state changes, quality changes and heartbeats)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS digital_telemetry (
    time    TIMESTAMPTZ NOT NULL,
    tag_id  TEXT        NOT NULL,
    state   BOOLEAN     NOT NULL,
    quality INTEGER     NOT NULL
);

SELECT create_hypertable('digital_telemetry', 'time',
                         chunk_time_interval => INTERVAL '7 days',
                         if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS digital_telemetry_tag_time_idx
    ON digital_telemetry (tag_id, time DESC);

ALTER TABLE digital_telemetry SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'tag_id',
    timescaledb.compress_orderby   = 'time DESC'
);
SELECT add_compression_policy('digital_telemetry', INTERVAL '30 days', if_not_exists => TRUE);

-- ---------------------------------------------------------------------------
-- Alarm history: one row per ISA-18.2 state transition
--   severity: 0 INFO, 1 LOW, 2 MEDIUM, 3 HIGH, 4 CRITICAL
--   state:    0 UNACK_ALARM, 1 ACK_ALARM, 2 UNACK_RTN, 3 ACK_RTN
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_history (
    time          TIMESTAMPTZ      NOT NULL,
    alarm_id      TEXT             NOT NULL,
    tag_id        TEXT             NOT NULL,
    severity      INTEGER          NOT NULL,
    state         INTEGER          NOT NULL,
    message       TEXT             NOT NULL,
    trigger_value DOUBLE PRECISION NOT NULL DEFAULT 0,
    ack_by_user   TEXT             NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS alarm_history_time_idx  ON alarm_history (time DESC);
CREATE INDEX IF NOT EXISTS alarm_history_alarm_idx ON alarm_history (alarm_id, time DESC);
