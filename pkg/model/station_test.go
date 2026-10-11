package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func findIssue(issues []Issue, sev, substr string) bool {
	for _, i := range issues {
		if i.Severity == sev && strings.Contains(i.Message, substr) {
			return true
		}
	}
	return false
}

func TestExampleStationIsValid(t *testing.T) {
	ex := ExampleStation("TEH_CGS_01", "Tehran CGS 01")
	issues := ex.Validate()
	if HasErrors(issues) {
		t.Fatalf("example has errors: %+v", issues)
	}
	for _, i := range issues {
		t.Logf("example warning: %+v", i)
	}
	tags := ex.BoundTags()
	want := []string{"TEH_CGS_01_INLET_VLV", "TEH_CGS_01_OUTLET_VLV", "TEH_CGS_01_P_IN", "TEH_CGS_01_P_OUT", "TEH_CGS_01_SLAM_SHUT", "TEH_CGS_01_TEMP"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Fatalf("bound tags %v, want %v", tags, want)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	ex := ExampleStation("S1", "S")
	data, err := json.Marshal(ex)
	if err != nil {
		t.Fatal(err)
	}
	var back StationTopology
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Components) != len(ex.Components) || len(back.Pipelines) != len(ex.Pipelines) || HasErrors(back.Validate()) {
		t.Fatalf("round trip changed the document")
	}
	if !strings.Contains(string(data), `"pipelines":[`) || strings.Contains(string(data), `"path_points":null`) {
		t.Fatalf("unexpected JSON shape: %s", data[:200])
	}
}

func TestValidationErrors(t *testing.T) {
	mut := func(f func(*StationTopology)) []Issue {
		ex := ExampleStation("S1", "S")
		f(ex)
		return ex.Validate()
	}
	cases := []struct {
		name string
		f    func(*StationTopology)
		want string
	}{
		{"bad station id", func(s *StationTopology) { s.StationID = "bad id" }, "station_id"},
		{"empty name", func(s *StationTopology) { s.Name = "" }, "name is required"},
		{"tiny canvas", func(s *StationTopology) { s.GridBounds.Width = 10 }, "width and height"},
		{"dup id", func(s *StationTopology) { s.Components[1].ID = s.Components[0].ID }, "duplicate id"},
		{"pipe id clashes with node id", func(s *StationTopology) { s.Pipelines[0].ID = "FL_IN" }, "duplicate id"},
		{"unknown type", func(s *StationTopology) { s.Components[0].Type = "WARP_DRIVE" }, "unknown equipment type"},
		{"bad rotation", func(s *StationTopology) { s.Components[0].Rotation = 45 }, "rotation"},
		{"outside canvas", func(s *StationTopology) { s.Components[0].X = 99999 }, "outside"},
		{"bad tag", func(s *StationTopology) { s.Components[3+1].Bindings.PrimaryTag = "a b;drop" }, "invalid tag id"},
		{"bad color rule op", func(s *StationTopology) { s.Components[4].Display.ColorRules[0].Op = "=~" }, "unknown operator"},
		{"bad color", func(s *StationTopology) { s.Components[4].Display.ColorRules[0].Color = "red" }, "#rrggbb"},
		{"missing node", func(s *StationTopology) { s.Pipelines[0].ToNodeID = "NOPE" }, "does not exist"},
		{"bad terminal", func(s *StationTopology) { s.Pipelines[0].FromTerminal = "in" }, "no terminal"},
		{"terminal reused", func(s *StationTopology) {
			s.Pipelines[1].FromTerminal = "in"
			s.Pipelines[1].FromNodeID = "BV_IN"
			s.Pipelines[1].ToNodeID = "FLT_01"
		}, "already used"},
		{"self loop", func(s *StationTopology) { s.Pipelines[0].ToNodeID = s.Pipelines[0].FromNodeID }, "to itself"},
		{"bad diameter", func(s *StationTopology) { s.Pipelines[0].NominalDiameter = "big" }, "nominal_diameter"},
		{"bend outside", func(s *StationTopology) { s.Pipelines[0].PathPoints = []Point{{X: -5, Y: 0}} }, "outside the canvas"},
		{"bands order", func(s *StationTopology) { s.PressureBands[1].UpTo = 5 }, "ascending"},
	}
	for _, c := range cases {
		issues := mut(c.f)
		if !findIssue(issues, "error", c.want) {
			t.Errorf("%s: expected an error containing %q, got %+v", c.name, c.want, issues)
		}
	}
}

func TestWarnings(t *testing.T) {
	ex := ExampleStation("S1", "S")
	ex.Components[1].Bindings.StateTag = ""   // valve without state
	ex.Components[4].Bindings.PrimaryTag = "" // transmitter without tag
	ex.Pipelines[0].BoundPressureTag = ""     // pipe without tag
	ex.Components = append(ex.Components, EquipmentNode{ID: "LONELY", Type: TypeFilter, X: 100, Y: 500})
	issues := ex.Validate()
	if HasErrors(issues) {
		t.Fatalf("unexpected errors: %+v", issues)
	}
	for _, w := range []string{"no state tag", "no primary tag", "no pressure tag", "not connected"} {
		if !findIssue(issues, "warning", w) {
			t.Errorf("missing warning %q in %+v", w, issues)
		}
	}
}

func TestNormalizeAndLimits(t *testing.T) {
	s := &StationTopology{StationID: "  X ", Name: " n "}
	s.Normalize()
	if s.StationID != "X" || s.Components == nil || s.Pipelines == nil {
		t.Fatalf("normalize: %+v", s)
	}
	big := ExampleStation("S1", "S")
	for i := 0; i < MaxComponents; i++ {
		big.Components = append(big.Components, EquipmentNode{ID: "N", Type: TypeFilter})
	}
	if !findIssue(big.Validate(), "error", "too many components") {
		t.Fatal("component limit not enforced")
	}
}
