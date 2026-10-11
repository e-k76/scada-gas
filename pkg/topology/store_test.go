package topology

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/example/scada-gas/pkg/model"
)

// storeSuite runs the same behavioural tests against every Store implementation.
func storeSuite(t *testing.T, s Store, station string) {
	ctx := context.Background()
	doc := model.ExampleStation(station, "Test station")

	if _, err := s.Get(ctx, station); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on empty store: %v", err)
	}
	if _, err := s.Save(ctx, doc, "alice", 5); !errors.Is(err, ErrConflict) {
		t.Fatalf("creating with a non-zero base version must conflict, got %v", err)
	}
	r1, err := s.Save(ctx, doc, "alice", 0)
	if err != nil || r1.Version != 1 || r1.Document.Version != 1 || r1.UpdatedBy != "alice" || r1.Document.UpdatedAtMs == 0 {
		t.Fatalf("first save: %+v %v", r1, err)
	}
	if _, err := s.Save(ctx, doc, "bob", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("saving over an existing station from version 0 must conflict, got %v", err)
	}

	edited := model.ExampleStation(station, "Renamed")
	edited.Components = edited.Components[:len(edited.Components)-1] // drop FL_OUT (pipe dangling is the caller's concern)
	r2, err := s.Save(ctx, edited, "bob", 1)
	if err != nil || r2.Version != 2 || r2.Name != "Renamed" {
		t.Fatalf("second save: %+v %v", r2, err)
	}
	var ce *ConflictError
	if _, err := s.Save(ctx, edited, "carol", 1); !errors.As(err, &ce) || ce.Current != 2 {
		t.Fatalf("stale save: %v", err)
	}

	got, err := s.Get(ctx, station)
	if err != nil || got.Version != 2 || len(got.Document.Components) != len(edited.Components) {
		t.Fatalf("Get latest: %+v %v", got, err)
	}
	old, err := s.GetVersion(ctx, station, 1)
	if err != nil || old.Name != "Test station" || len(old.Components) != len(doc.Components) {
		t.Fatalf("GetVersion(1): %+v %v", old, err)
	}
	if _, err := s.GetVersion(ctx, station, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetVersion(99): %v", err)
	}

	if err := s.MarkDeployed(ctx, station, 1, "dave"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDeployed(ctx, station, 7, "dave"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deploying a future version: %v", err)
	}
	if err := s.MarkDeployed(ctx, "NOPE", 1, "dave"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deploying unknown station: %v", err)
	}
	got, _ = s.Get(ctx, station)
	if got.DeployedVersion != 1 || got.DeployedBy != "dave" || got.DeployedAt == nil {
		t.Fatalf("deployment not recorded: %+v", got)
	}
	// saving a new version keeps the deployed bookkeeping
	r3, err := s.Save(ctx, edited, "bob", 2)
	if err != nil || r3.DeployedVersion != 1 {
		t.Fatalf("deployed info lost on save: %+v %v", r3, err)
	}

	list, err := s.List(ctx)
	found := false
	for _, x := range list {
		if x.StationID == station && x.Version == 3 && x.DeployedVersion == 1 {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatalf("List: %+v %v", list, err)
	}

	// concurrent creators of the same new station: exactly one wins
	other := "RACE_" + station
	var wg sync.WaitGroup
	var wins, conflicts int
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Save(ctx, model.ExampleStation(other, "race"), "x", 0)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if errors.Is(err, ErrConflict) {
				conflicts++
			} else {
				t.Errorf("race: unexpected error %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 7 {
		t.Fatalf("race: %d wins, %d conflicts", wins, conflicts)
	}
}

func TestMemoryStore(t *testing.T) { storeSuite(t, NewMemoryStore(), "MEM_ST") }

// TestPGStore runs against a real database when TEST_PG_DSN is set, e.g.
// TEST_PG_DSN=postgres://scada:scada@127.0.0.1:5432/scada?sslmode=disable
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
	station := "PGTEST_" + os.Getenv("TEST_PG_SUFFIX")
	if _, err := s.pool.Exec(ctx, `DELETE FROM station_topology WHERE station_id LIKE 'PGTEST%' OR station_id LIKE 'RACE_PGTEST%'`); err != nil {
		// schema may not exist yet on the first run: ensure creates it
		if e := s.ensure(ctx); e != nil {
			t.Fatal(e)
		}
		_, _ = s.pool.Exec(ctx, `DELETE FROM station_topology WHERE station_id LIKE 'PGTEST%' OR station_id LIKE 'RACE_PGTEST%'`)
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM station_topology_history WHERE station_id LIKE 'PGTEST%' OR station_id LIKE 'RACE_PGTEST%'`)
	storeSuite(t, s, station)
}
