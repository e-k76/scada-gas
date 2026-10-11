package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
	"github.com/example/scada-gas/pkg/model"
	"github.com/example/scada-gas/pkg/topology"
)

// Deployer publishes a topology's runtime configuration to the core, which in
// turn notifies the FEPs. *GRPCCore implements it.
type Deployer interface {
	DeployTopology(ctx context.Context, req *scadav1.DeployTopologyRequest) (*scadav1.DeployTopologyResponse, error)
	RuntimeConfigs(ctx context.Context) ([]*scadav1.RuntimeConfig, error)
}

const maxTopologyBytes = 1 << 20

// TopologyAPI serves the Engineering Studio endpoints:
//
//	GET  /api/v1/topology                       list stations
//	GET  /api/v1/topology/{station_id}          fetch the saved drawing
//	                                            (?version=deployed: the drawing that is running)
//	POST /api/v1/topology/{station_id}          save (create or update)
//	POST /api/v1/topology/{station_id}/deploy   publish to the runtime
//	GET  /api/v1/topology/{station_id}/example  a ready-made example drawing
type TopologyAPI struct {
	store    topology.Store
	deployer Deployer
	timeout  time.Duration
	recon    sync.Mutex
}

// NewTopologyAPI creates the handlers.
func NewTopologyAPI(store topology.Store, deployer Deployer) *TopologyAPI {
	return &TopologyAPI{store: store, deployer: deployer, timeout: 10 * time.Second}
}

// Register adds the routes to mux. read wraps the read-only endpoints
// (authentication), write the endpoints that change or deploy a topology
// (authentication plus the engineer role).
func (t *TopologyAPI) Register(mux *http.ServeMux, read, write func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/topology", read(http.HandlerFunc(t.handleList)))
	mux.Handle("GET /api/v1/topology/{station_id}", read(http.HandlerFunc(t.handleGet)))
	mux.Handle("POST /api/v1/topology/{station_id}", write(http.HandlerFunc(t.handleSave)))
	mux.Handle("POST /api/v1/topology/{station_id}/deploy", write(http.HandlerFunc(t.handleDeploy)))
	mux.Handle("GET /api/v1/topology/{station_id}/example", read(http.HandlerFunc(t.handleExample)))
}

// --- JSON shapes ---------------------------------------------------------------

type topologyStatus struct {
	Version            int64  `json:"version"`
	UpdatedAtMs        int64  `json:"updated_at_ms"`
	UpdatedBy          string `json:"updated_by,omitempty"`
	DeployedVersion    int64  `json:"deployed_version"`
	DeployedAtMs       int64  `json:"deployed_at_ms,omitempty"`
	DeployedBy         string `json:"deployed_by,omitempty"`
	ChangedSinceDeploy bool   `json:"changed_since_deploy"`
}

func statusOf(r *topology.Record) topologyStatus {
	s := topologyStatus{
		Version: r.Version, UpdatedAtMs: r.UpdatedAt.UnixMilli(), UpdatedBy: r.UpdatedBy,
		DeployedVersion: r.DeployedVersion, DeployedBy: r.DeployedBy,
		ChangedSinceDeploy: r.DeployedVersion == 0 || r.Version > r.DeployedVersion,
	}
	if r.DeployedAt != nil {
		s.DeployedAtMs = r.DeployedAt.UnixMilli()
	}
	return s
}

type stationSummary struct {
	StationID string `json:"station_id"`
	Name      string `json:"name"`
	topologyStatus
}

// --- helpers -------------------------------------------------------------------

func stationFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("station_id")
	if !model.ValidStationID(id) {
		writeError(w, http.StatusBadRequest, "invalid station_id")
		return "", false
	}
	return id, true
}

func writeStoreError(w http.ResponseWriter, err error) {
	var conflict *topology.ConflictError
	switch {
	case errors.Is(err, topology.ErrNotFound):
		writeError(w, http.StatusNotFound, "station topology not found")
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":           conflict.Error(),
			"current_version": conflict.Current,
		})
	default:
		log.Printf("[GW] topology store error: %v", err)
		writeError(w, http.StatusServiceUnavailable, "topology database unavailable")
	}
}

func decodeStrict(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxTopologyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		}
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "request body must contain a single JSON object")
		return false
	}
	return true
}

// --- handlers ------------------------------------------------------------------

func (t *TopologyAPI) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), t.timeout)
	defer cancel()
	list, err := t.store.List(ctx)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	out := make([]stationSummary, 0, len(list))
	for _, s := range list {
		rec := &topology.Record{Version: s.Version, UpdatedAt: s.UpdatedAt, UpdatedBy: s.UpdatedBy,
			DeployedVersion: s.DeployedVersion, DeployedAt: s.DeployedAt, DeployedBy: s.DeployedBy}
		out = append(out, stationSummary{StationID: s.StationID, Name: s.Name, topologyStatus: statusOf(rec)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"stations": out})
}

func (t *TopologyAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := stationFromPath(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), t.timeout)
	defer cancel()
	rec, err := t.store.Get(ctx, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	doc := rec.Document
	if v := r.URL.Query().Get("version"); v != "" {
		if v != "deployed" {
			writeError(w, http.StatusBadRequest, `version must be "deployed" (or omitted for the latest saved drawing)`)
			return
		}
		if rec.DeployedVersion == 0 {
			writeError(w, http.StatusNotFound, "this station has never been deployed")
			return
		}
		if doc, err = t.store.GetVersion(ctx, id, rec.DeployedVersion); err != nil {
			writeStoreError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"topology": doc, "status": statusOf(rec), "issues": doc.Validate(),
	})
}

func (t *TopologyAPI) handleExample(w http.ResponseWriter, r *http.Request) {
	id, ok := stationFromPath(w, r)
	if !ok {
		return
	}
	ex := model.ExampleStation(id, "Example city gate station "+id)
	writeJSON(w, http.StatusOK, map[string]any{"topology": ex, "issues": ex.Validate()})
}

func (t *TopologyAPI) handleSave(w http.ResponseWriter, r *http.Request) {
	id, ok := stationFromPath(w, r)
	if !ok {
		return
	}
	var doc model.StationTopology
	if !decodeStrict(w, r, &doc) {
		return
	}
	doc.Normalize()
	if doc.StationID == "" {
		doc.StationID = id
	}
	if doc.StationID != id {
		writeError(w, http.StatusBadRequest, "station_id in the body does not match the URL")
		return
	}
	issues := doc.Validate()
	if n := model.ErrorCount(issues); n > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": plural(n, "validation error") + ", nothing was saved", "issues": issues,
		})
		return
	}
	by := strings.TrimSpace(doc.UpdatedBy)
	if p := PrincipalFrom(r.Context()); p.Named() {
		by = p.User // the credentials decide the author, not the request body
	}
	if !validUser(by) {
		writeError(w, http.StatusBadRequest, "updated_by is required (1-64 printable characters): enter your operator name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), t.timeout)
	defer cancel()
	rec, err := t.store.Save(ctx, &doc, by, doc.Version)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	log.Printf("[GW] topology %s saved as v%d by %q (%d components, %d pipelines, %d warnings)",
		id, rec.Version, by, len(doc.Components), len(doc.Pipelines), len(issues))
	writeJSON(w, http.StatusOK, map[string]any{"topology": rec.Document, "status": statusOf(rec), "issues": issues})
}

type deployRequest struct {
	DeployedBy string `json:"deployed_by"`
}

func (t *TopologyAPI) handleDeploy(w http.ResponseWriter, r *http.Request) {
	id, ok := stationFromPath(w, r)
	if !ok {
		return
	}
	var req deployRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	req.DeployedBy = strings.TrimSpace(req.DeployedBy)
	if p := PrincipalFrom(r.Context()); p.Named() {
		req.DeployedBy = p.User
	}
	if !validUser(req.DeployedBy) {
		writeError(w, http.StatusBadRequest, "deployed_by is required (1-64 printable characters)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), t.timeout)
	defer cancel()
	rec, err := t.store.Get(ctx, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	doc := rec.Document
	issues := doc.Validate()
	if n := model.ErrorCount(issues); n > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": "the saved topology has " + plural(n, "validation error") + ", fix and save it first", "issues": issues,
		})
		return
	}
	tags := doc.BoundTags()
	if len(tags) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no SCADA tags are bound in this topology, there is nothing to deploy")
		return
	}
	resp, err := t.deployer.DeployTopology(ctx, &scadav1.DeployTopologyRequest{
		StationId: id, Version: rec.Version, DeployedBy: req.DeployedBy, BoundTags: tags,
		ComponentCount: int32(len(doc.Components)), PipelineCount: int32(len(doc.Pipelines)),
	})
	if err != nil {
		writeCoreError(w, err)
		return
	}
	if !resp.GetAccepted() {
		writeJSON(w, http.StatusConflict, map[string]any{"deployed": false, "error": resp.GetMessage()})
		return
	}
	if err := t.store.MarkDeployed(ctx, id, rec.Version, req.DeployedBy); err != nil {
		log.Printf("[GW] topology %s v%d deployed but not recorded: %v", id, rec.Version, err)
		writeStoreError(w, err)
		return
	}
	log.Printf("[GW] topology %s v%d deployed by %q: %d tags (%d live, %d unresolved), %d subscriber(s) notified",
		id, rec.Version, req.DeployedBy, len(tags), len(resp.GetLiveTags()), len(resp.GetUnresolvedTags()), resp.GetSubscribersNotified())
	writeJSON(w, http.StatusOK, map[string]any{
		"deployed": true, "version": rec.Version, "deployed_by": req.DeployedBy,
		"bound_tags": tags, "live_tags": nonNil(resp.GetLiveTags()), "unresolved_tags": nonNil(resp.GetUnresolvedTags()),
		"subscribers_notified": resp.GetSubscribersNotified(), "issues": issues,
	})
}

// Reconcile re-publishes every deployed topology the core does not know about
// (for example after the core restarted, which loses its in-memory configs).
func (t *TopologyAPI) Reconcile(ctx context.Context) {
	if !t.recon.TryLock() {
		return
	}
	defer t.recon.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	list, err := t.store.List(ctx)
	if err != nil {
		log.Printf("[GW] topology reconcile: %v", err)
		return
	}
	known, err := t.deployer.RuntimeConfigs(ctx)
	if err != nil {
		log.Printf("[GW] topology reconcile: core: %v", err)
		return
	}
	have := map[string]int64{}
	for _, c := range known {
		have[c.GetStationId()] = c.GetVersion()
	}
	for _, s := range list {
		if s.DeployedVersion == 0 || have[s.StationID] >= s.DeployedVersion {
			continue
		}
		doc, err := t.store.GetVersion(ctx, s.StationID, s.DeployedVersion)
		if err != nil {
			log.Printf("[GW] topology reconcile: %s v%d: %v", s.StationID, s.DeployedVersion, err)
			continue
		}
		by := s.DeployedBy
		if by == "" {
			by = "gateway-reconcile"
		}
		resp, err := t.deployer.DeployTopology(ctx, &scadav1.DeployTopologyRequest{
			StationId: s.StationID, Version: s.DeployedVersion, DeployedBy: by, BoundTags: doc.BoundTags(),
			ComponentCount: int32(len(doc.Components)), PipelineCount: int32(len(doc.Pipelines)),
		})
		if err != nil || !resp.GetAccepted() {
			log.Printf("[GW] topology reconcile: %s v%d not accepted: %v %s", s.StationID, s.DeployedVersion, err, resp.GetMessage())
			continue
		}
		log.Printf("[GW] topology reconcile: re-published %s v%d to the core", s.StationID, s.DeployedVersion)
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
