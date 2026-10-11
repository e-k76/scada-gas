-- Audit log of supervisory control commands (plain PostgreSQL, also valid on
-- TimescaleDB). Idempotent: the gateway applies it automatically on first use
-- and docker-compose runs it on first boot.
--
-- Every command attempt produces exactly one row, including refused ones.
-- A row is inserted BEFORE the command is dispatched (status PENDING) and
-- completed afterwards; a PENDING row that stays PENDING means the gateway
-- died mid-command and the outcome is unknown. Completed rows are immutable
-- and no row can be deleted (enforced by the trigger below).

CREATE TABLE IF NOT EXISTS command_audit_log (
    id             BIGSERIAL        PRIMARY KEY,
    time           TIMESTAMPTZ      NOT NULL DEFAULT now(),  -- when the command was requested
    command_id     TEXT             NOT NULL,
    operator       TEXT             NOT NULL,                -- authenticated user ('' = unauthenticated attempt)
    role           TEXT             NOT NULL DEFAULT '',
    remote_addr    TEXT             NOT NULL DEFAULT '',
    station_id     TEXT             NOT NULL,
    tag_id         TEXT             NOT NULL,
    action         TEXT             NOT NULL,
    setpoint_value DOUBLE PRECISION,
    -- PENDING | EXECUTED | FAILED | REJECTED | NO_FEP | TIMEOUT | DENIED
    status         TEXT             NOT NULL,
    success        BOOLEAN          NOT NULL DEFAULT false,
    message        TEXT             NOT NULL DEFAULT '',
    executed_at    TIMESTAMPTZ,                              -- when the field device executed it
    latency_ms     INTEGER
);

CREATE INDEX IF NOT EXISTS command_audit_log_time_idx    ON command_audit_log (time DESC);
CREATE INDEX IF NOT EXISTS command_audit_log_tag_idx     ON command_audit_log (tag_id, time DESC);
CREATE INDEX IF NOT EXISTS command_audit_log_command_idx ON command_audit_log (command_id);

CREATE OR REPLACE FUNCTION command_audit_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'command_audit_log is append-only: rows cannot be deleted';
    END IF;
    IF OLD.status <> 'PENDING' THEN
        RAISE EXCEPTION 'command_audit_log: a completed record (status %) cannot be modified', OLD.status;
    END IF;
    IF NEW.id <> OLD.id OR NEW.time <> OLD.time OR NEW.command_id <> OLD.command_id
       OR NEW.operator <> OLD.operator OR NEW.role <> OLD.role OR NEW.remote_addr <> OLD.remote_addr
       OR NEW.station_id <> OLD.station_id OR NEW.tag_id <> OLD.tag_id OR NEW.action <> OLD.action
       OR NEW.setpoint_value IS DISTINCT FROM OLD.setpoint_value THEN
        RAISE EXCEPTION 'command_audit_log: only the outcome columns of a pending record can be updated';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS command_audit_guard_trg ON command_audit_log;
CREATE TRIGGER command_audit_guard_trg
    BEFORE UPDATE OR DELETE ON command_audit_log
    FOR EACH ROW EXECUTE FUNCTION command_audit_guard();
