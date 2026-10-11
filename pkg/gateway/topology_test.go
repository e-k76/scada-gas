package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/model"
	"github.com/example/scada-gas/pkg/topology"
)

type fakeDeployer struct {
	mu       sync.Mutex
	reqs     []*scadav1.DeployTopologyRequest
	resp     *scadav1.DeployTopologyResponse
	err      error
	known    []*scadav1.RuntimeConfig
	knownErr error
}

func (f *fakeDeployer) DeployTopology(_ context.Context, r *scadav1.DeployTopologyRequest) (*scadav1.DeployTopologyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r)
	return f.resp, f.err
}
func (f *fakeDeployer) RuntimeConfigs(context.Context) ([]*scadav1.RuntimeConfig, error) {
	return f.known, f.knownErr
}

func topoServer(t *testing.T, store topology.Store, dep Deployer, token string) *httptest.Server {
	t.Helper()
	core := newFakeCore()
	api := NewAPI(APIConfig{Core: core, Topology: NewTopologyAPI(store, dep), Token: token})
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func exampleBody(t *testing.T, station string, version int64, mutate func(*model.StationTopology)) string {
	t.Helper()
	ex := model.ExampleStation(station, "Test")
	ex.Version = version
	ex.UpdatedBy = "alice"
	if mutate != nil {
		mutate(ex)
	}
	b, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTopologySaveGetListConflict(t *testing.T) {
	srv := topoServer(t, topology.NewMemoryStore(), &fakeDeployer{}, "")
	base := srv.URL + "/api/v1/topology"

	if resp, _ := do(t, "GET", base+"/ST1", "", nil); resp.StatusCode != 404 {
		t.Fatalf("missing station: %d", resp.StatusCode)
	}
	resp, m := do(t, "POST", base+"/ST1", exampleBody(t, "ST1", 0, nil), nil)
	if resp.StatusCode != 200 || m["topology"].(map[string]any)["version"].(float64) != 1 {
		t.Fatalf("create: %d %v", resp.StatusCode, m)
	}
	resp, m = do(t, "GET", base+"/ST1", "", nil)
	topo := m["topology"].(map[string]any)
	if resp.StatusCode != 200 || len(topo["components"].([]any)) != 14 || m["status"].(map[string]any)["changed_since_deploy"] != true {
		t.Fatalf("get: %d %v", resp.StatusCode, m["status"])
	}
	// update from v1 works, from v1 again conflicts
	if resp, m = do(t, "POST", base+"/ST1", exampleBody(t, "ST1", 1, nil), nil); resp.StatusCode != 200 || m["status"].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("update: %d %v", resp.StatusCode, m)
	}
	resp, m = do(t, "POST", base+"/ST1", exampleBody(t, "ST1", 1, nil), nil)
	if resp.StatusCode != 409 || m["current_version"].(float64) != 2 {
		t.Fatalf("conflict: %d %v", resp.StatusCode, m)
	}
	_, m = do(t, "GET", base, "", nil)
	list := m["stations"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["station_id"] != "ST1" || list[0].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("list: %v", m)
	}
}

func TestTopologySaveValidation(t *testing.T) {
	srv := topoServer(t, topology.NewMemoryStore(), &fakeDeployer{}, "")
	url := srv.URL + "/api/v1/topology/ST1"

	resp, m := do(t, "POST", url, exampleBody(t, "ST1", 0, func(s *model.StationTopology) { s.Pipelines[0].ToNodeID = "GHOST" }), nil)
	issues, _ := m["issues"].([]any)
	if resp.StatusCode != 422 || len(issues) == 0 || !strings.Contains(m["error"].(string), "validation error") {
		t.Fatalf("invalid topology: %d %v", resp.StatusCode, m)
	}
	if resp, _ := do(t, "GET", url, "", nil); resp.StatusCode != 404 {
		t.Fatal("an invalid topology must not be stored")
	}
	if resp, _ := do(t, "POST", url, exampleBody(t, "OTHER", 0, nil), nil); resp.StatusCode != 400 {
		t.Fatalf("station id mismatch: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", url, exampleBody(t, "ST1", 0, func(s *model.StationTopology) { s.UpdatedBy = "" }), nil); resp.StatusCode != 400 {
		t.Fatalf("missing updated_by: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", url, `{"station_id":"ST1","bogus":1}`, nil); resp.StatusCode != 400 {
		t.Fatalf("unknown field: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", url, `nope`, nil); resp.StatusCode != 400 {
		t.Fatalf("garbage: %d", resp.StatusCode)
	}
	big := exampleBody(t, "ST1", 0, func(s *model.StationTopology) { s.Description = strings.Repeat("x", 2<<20) })
	if resp, _ := do(t, "POST", url, big, nil); resp.StatusCode != 413 {
		t.Fatalf("oversize: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", srv.URL+"/api/v1/topology/bad%20id", "", nil); resp.StatusCode != 400 {
		t.Fatalf("bad path id: %d", resp.StatusCode)
	}
}

func TestTopologyExample(t *testing.T) {
	srv := topoServer(t, topology.NewMemoryStore(), &fakeDeployer{}, "")
	resp, m := do(t, "GET", srv.URL+"/api/v1/topology/XYZ_01/example", "", nil)
	topo := m["topology"].(map[string]any)
	if resp.StatusCode != 200 || topo["station_id"] != "XYZ_01" || !strings.Contains(string(mustJSON(t, topo)), "XYZ_01_P_IN") {
		t.Fatalf("example: %d %v", resp.StatusCode, m)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTopologyDeploy(t *testing.T) {
	dep := &fakeDeployer{resp: &scadav1.DeployTopologyResponse{
		Accepted: true, Message: "configuration applied", Version: 1,
		LiveTags: []string{"ST1_P_IN"}, UnresolvedTags: []string{"ST1_TEMP"}, SubscribersNotified: 2,
	}}
	store := topology.NewMemoryStore()
	srv := topoServer(t, store, dep, "")
	base := srv.URL + "/api/v1/topology/ST1"
	dbody := `{"deployed_by":"erin"}`

	if resp, _ := do(t, "POST", base+"/deploy", dbody, nil); resp.StatusCode != 404 {
		t.Fatalf("deploy before save: %d", resp.StatusCode)
	}
	do(t, "POST", base, exampleBody(t, "ST1", 0, nil), nil)
	if resp, _ := do(t, "POST", base+"/deploy", `{"deployed_by":""}`, nil); resp.StatusCode != 400 {
		t.Fatalf("missing user: %d", resp.StatusCode)
	}
	resp, m := do(t, "POST", base+"/deploy", dbody, nil)
	if resp.StatusCode != 200 || m["deployed"] != true || m["subscribers_notified"].(float64) != 2 || len(m["bound_tags"].([]any)) != 7 {
		t.Fatalf("deploy: %d %v", resp.StatusCode, m)
	}
	if len(dep.reqs) != 1 || dep.reqs[0].Version != 1 || dep.reqs[0].DeployedBy != "erin" || dep.reqs[0].ComponentCount != 14 {
		t.Fatalf("core request: %+v", dep.reqs)
	}
	rec, _ := store.Get(context.Background(), "ST1")
	if rec.DeployedVersion != 1 || rec.DeployedBy != "erin" {
		t.Fatalf("deployment not recorded: %+v", rec)
	}
	_, m = do(t, "GET", base, "", nil)
	if m["status"].(map[string]any)["changed_since_deploy"] != false {
		t.Fatalf("status after deploy: %v", m["status"])
	}

	dep.err = status.Error(codes.Unavailable, "down")
	if resp, _ := do(t, "POST", base+"/deploy", dbody, nil); resp.StatusCode != 503 {
		t.Fatalf("core down: %d", resp.StatusCode)
	}
	dep.err = nil
	dep.resp = &scadav1.DeployTopologyResponse{Accepted: false, Message: "stale version"}
	if resp, m := do(t, "POST", base+"/deploy", dbody, nil); resp.StatusCode != 409 || m["deployed"] != false {
		t.Fatalf("rejected: %d %v", resp.StatusCode, m)
	}
}

func TestTopologyDeployRefusesInvalidOrEmpty(t *testing.T) {
	dep := &fakeDeployer{resp: &scadav1.DeployTopologyResponse{Accepted: true}}
	store := topology.NewMemoryStore()
	srv := topoServer(t, store, dep, "")
	// a topology without any tag binding (valid, but nothing to deploy)
	empty := exampleBody(t, "EMPTY", 0, func(s *model.StationTopology) {
		for i := range s.Components {
			s.Components[i].Bindings = model.TagBindings{}
		}
		for i := range s.Pipelines {
			s.Pipelines[i].BoundPressureTag = ""
		}
	})
	if resp, _ := do(t, "POST", srv.URL+"/api/v1/topology/EMPTY", empty, nil); resp.StatusCode != 200 {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", srv.URL+"/api/v1/topology/EMPTY/deploy", `{"deployed_by":"x"}`, nil); resp.StatusCode != 422 {
		t.Fatalf("empty deploy: %d", resp.StatusCode)
	}
	if len(dep.reqs) != 0 {
		t.Fatal("the core must not be called")
	}
}

func TestTopologyAuth(t *testing.T) {
	srv := topoServer(t, topology.NewMemoryStore(), &fakeDeployer{}, "tok")
	if resp, _ := do(t, "GET", srv.URL+"/api/v1/topology", "", nil); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "POST", srv.URL+"/api/v1/topology/ST1", exampleBody(t, "ST1", 0, nil), nil); resp.StatusCode != 401 {
		t.Fatalf("no token on save: %d", resp.StatusCode)
	}
	if resp, _ := do(t, "GET", srv.URL+"/api/v1/topology", "", map[string]string{"Authorization": "Bearer tok"}); resp.StatusCode != 200 {
		t.Fatalf("with token: %d", resp.StatusCode)
	}
}

func TestReconcileRepublishesMissingConfigs(t *testing.T) {
	store := topology.NewMemoryStore()
	ctx := context.Background()
	doc := model.ExampleStation("ST1", "x")
	if _, err := store.Save(ctx, doc, "a", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, doc, "a", 1); err != nil { // v2 saved but only v1 deployed
		t.Fatal(err)
	}
	_ = store.MarkDeployed(ctx, "ST1", 1, "ops")
	other := model.ExampleStation("ST2", "y")
	_, _ = store.Save(ctx, other, "a", 0) // never deployed: must be skipped

	dep := &fakeDeployer{resp: &scadav1.DeployTopologyResponse{Accepted: true}}
	api := NewTopologyAPI(store, dep)

	api.Reconcile(ctx)
	if len(dep.reqs) != 1 || dep.reqs[0].StationId != "ST1" || dep.reqs[0].Version != 1 || dep.reqs[0].DeployedBy != "ops" || len(dep.reqs[0].BoundTags) != 7 {
		t.Fatalf("reconcile requests: %+v", dep.reqs)
	}
	dep.reqs = nil
	dep.known = []*scadav1.RuntimeConfig{{StationId: "ST1", Version: 1}}
	api.Reconcile(ctx)
	if len(dep.reqs) != 0 {
		t.Fatalf("an up-to-date core must not be touched: %+v", dep.reqs)
	}
	dep.knownErr = status.Error(codes.Unavailable, "down")
	api.Reconcile(ctx) // must only log
	time.Sleep(10 * time.Millisecond)
}

var _ = http.StatusOK
