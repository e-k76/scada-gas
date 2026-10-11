package audit

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func suite(t *testing.T, s Store) {
	ctx := context.Background()
	sp := 62.5
	id, err := s.Begin(ctx, Entry{CommandID: "cmd-0001", Operator: "alice", Role: "operator", RemoteAddr: "10.0.0.1:5555",
		StationID: "ST", TagID: "ST_P_OUT_SP", Action: "CMD_SET_SETPOINT", Setpoint: &sp, Status: Pending})
	if err != nil || id == 0 {
		t.Fatalf("Begin: %d %v", id, err)
	}
	now := time.Now().UTC()
	if err := s.Complete(ctx, id, Result{Status: Executed, Success: true, Message: "ok", ExecutedAt: &now, LatencyMs: 42}); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, id, Result{Status: Failed}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a completed entry must not be completed again: %v", err)
	}
	denied, err := s.Begin(ctx, Entry{CommandID: "cmd-0002", Operator: "", StationID: "ST", TagID: "ST_INLET_VLV",
		Action: "CMD_CLOSE_VALVE", Status: Denied, Message: "missing token"})
	if err != nil || denied == id {
		t.Fatalf("denied entry: %d %v", denied, err)
	}
	if err := s.Complete(ctx, denied, Result{Status: Executed}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("a DENIED entry can never be completed: %v", err)
	}
	long := strings.Repeat("x", 5000)
	if _, err := s.Begin(ctx, Entry{CommandID: long, Operator: long, StationID: long, TagID: long, Action: long, Message: long, Status: Denied}); err != nil {
		t.Fatalf("oversized fields must be clipped, not fail: %v", err)
	}

	all, err := s.Recent(ctx, "", 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("Recent: %d %v", len(all), err)
	}
	if len(all[0].Operator) != 64 || len(all[0].Message) != 1000 {
		t.Fatalf("clipping: %d %d", len(all[0].Operator), len(all[0].Message))
	}
	byTag, _ := s.Recent(ctx, "ST_P_OUT_SP", 10)
	if len(byTag) != 1 || byTag[0].Status != Executed || !byTag[0].Success || byTag[0].Operator != "alice" ||
		byTag[0].Setpoint == nil || *byTag[0].Setpoint != 62.5 || byTag[0].LatencyMs == nil || *byTag[0].LatencyMs != 42 || byTag[0].ExecutedAt == nil {
		t.Fatalf("stored entry: %+v", byTag)
	}
	if one, _ := s.Recent(ctx, "", 1); len(one) != 1 || one[0].ID != all[0].ID {
		t.Fatalf("limit/order: %+v", one)
	}
}

func TestMemoryStore(t *testing.T) { suite(t, NewMemoryStore()) }

// TestPGStore needs TEST_PG_DSN, e.g. postgres://scada:scada@127.0.0.1:5432/scada?sslmode=disable
func TestPGStore(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		t.Skip("TEST_PG_DSN not set")
	}
	ctx := context.Background()
	s, err := NewPGStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.ensure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `TRUNCATE command_audit_log`); err != nil { // TRUNCATE is not blocked by the row trigger
		t.Fatal(err)
	}
	suite(t, s)

	// the database itself enforces append-only semantics
	if _, err := s.pool.Exec(ctx, `DELETE FROM command_audit_log`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("DELETE must be refused by the trigger, got %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE command_audit_log SET message = 'tampered' WHERE command_id = 'cmd-0001'`); err == nil || !strings.Contains(err.Error(), "cannot be modified") {
		t.Fatalf("UPDATE of a completed record must be refused, got %v", err)
	}
	id, err := s.Begin(ctx, Entry{CommandID: "cmd-0003", Operator: "bob", StationID: "ST", TagID: "T", Action: "A", Status: Pending})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE command_audit_log SET operator = 'mallory' WHERE id = $1`, id); err == nil || !strings.Contains(err.Error(), "only the outcome columns") {
		t.Fatalf("changing the operator of a pending record must be refused, got %v", err)
	}
	if err := s.Complete(ctx, id, Result{Status: Timeout, Message: "unknown"}); err != nil {
		t.Fatal(err)
	}
}
