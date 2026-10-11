// Package model defines the station topology (single-line diagram / P&ID)
// created in the Engineering Studio: equipment nodes with tag bindings and
// the pipelines connecting them, plus validation of the whole document.
package model

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Equipment types.
const (
	TypeBallValve     = "BALL_VALVE"
	TypeSlamShutValve = "SLAM_SHUT_VALVE"
	TypePressureTx    = "PRESSURE_TRANSMITTER"
	TypeTemperatureTx = "TEMPERATURE_TRANSMITTER"
	TypeFilter        = "FILTER"
	TypeRegulator     = "REGULATOR"
	TypeOdorizer      = "ODORIZER"
	TypePressureGauge = "PRESSURE_GAUGE"
	TypeJunction      = "JUNCTION"
	TypeFlange        = "FLANGE"
)

// terminalsByType lists the connection points of each equipment type. The
// geometry (where a terminal sits relative to the symbol) lives in the editor;
// the names must stay in sync with web/editor.js.
//
//	two-port inline equipment: "in" (left at rotation 0) and "out" (right)
//	instruments:               "port" (process connection at the bottom)
//	JUNCTION (tee/cross):      "w", "e", "n", "s"
//	FLANGE (station boundary): "port"
var terminalsByType = map[string][]string{
	TypeBallValve:     {"in", "out"},
	TypeSlamShutValve: {"in", "out"},
	TypeFilter:        {"in", "out"},
	TypeRegulator:     {"in", "out"},
	TypeOdorizer:      {"in", "out"},
	TypePressureTx:    {"port"},
	TypeTemperatureTx: {"port"},
	TypePressureGauge: {"port"},
	TypeJunction:      {"w", "e", "n", "s"},
	TypeFlange:        {"port"},
}

// Limits protecting the database and the browser.
const (
	MaxComponents = 500
	MaxPipelines  = 1000
	MaxPathPoints = 64
	MaxColorRules = 8
	MaxBands      = 8
	minGrid       = 5
	maxGrid       = 100
	minCanvas     = 200
	maxCanvas     = 10000
)

var (
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,64}$`)
	stationPattern  = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,64}$`)
	tagPattern      = regexp.MustCompile(`^[A-Za-z0-9_.:\-]{1,128}$`)
	colorPattern    = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	diameterPattern = regexp.MustCompile(`^(\d{1,3}(\.\d)?-inch|DN\d{2,4})$`)
)

var validOps = map[string]bool{">": true, ">=": true, "<": true, "<=": true, "==": true, "!=": true}

// ValidStationID reports whether s is an acceptable station identifier.
func ValidStationID(s string) bool { return stationPattern.MatchString(s) }

// ValidTagID reports whether s is an acceptable SCADA tag identifier.
func ValidTagID(s string) bool { return tagPattern.MatchString(s) }

// IsKnownType reports whether t is a supported equipment type.
func IsKnownType(t string) bool { _, ok := terminalsByType[t]; return ok }

// Terminals returns the terminal names of an equipment type.
func Terminals(t string) []string { return append([]string(nil), terminalsByType[t]...) }

// GridBounds is the size of the drawing canvas in pixels and its snap grid.
type GridBounds struct {
	Width    int `json:"width"`
	Height   int `json:"height"`
	GridSize int `json:"grid_size"`
}

// Point is a canvas coordinate.
type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// TagBindings maps an element to SCADA tags.
type TagBindings struct {
	PrimaryTag string `json:"primary_tag,omitempty"` // measured value (analog)
	StateTag   string `json:"state_tag,omitempty"`   // state / alarm (digital)
}

// ColorRule colours an element when its bound value satisfies Op Value.
// For digital tags the value is compared as 1 (true) / 0 (false).
type ColorRule struct {
	Op    string  `json:"op"`
	Value float64 `json:"value"`
	Color string  `json:"color"`
}

// DisplaySettings are presentation options of an element.
type DisplaySettings struct {
	Label       string      `json:"label,omitempty"`        // text shown instead of the id
	LabelPrefix string      `json:"label_prefix,omitempty"` // shown before live values
	Unit        string      `json:"unit,omitempty"`         // overrides the tag's unit
	Decimals    *int        `json:"decimals,omitempty"`
	InvertState bool        `json:"invert_state,omitempty"` // true state means closed
	ColorRules  []ColorRule `json:"color_rules,omitempty"`
}

// EquipmentNode is one symbol placed on the canvas.
type EquipmentNode struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	X        int             `json:"x"`
	Y        int             `json:"y"`
	Rotation int             `json:"rotation"` // 0, 90, 180, 270
	Bindings TagBindings     `json:"bindings"`
	Display  DisplaySettings `json:"display"`
}

// PipelineSegment connects two equipment terminals.
type PipelineSegment struct {
	ID               string  `json:"id"`
	FromNodeID       string  `json:"from_node_id"`
	FromTerminal     string  `json:"from_terminal"`
	ToNodeID         string  `json:"to_node_id"`
	ToTerminal       string  `json:"to_terminal"`
	PathPoints       []Point `json:"path_points"` // intermediate bends
	NominalDiameter  string  `json:"nominal_diameter"`
	BoundPressureTag string  `json:"bound_pressure_tag,omitempty"`
}

// PressureBand colours pipes whose bound pressure is <= UpTo (bands are
// evaluated in ascending order).
type PressureBand struct {
	UpTo  float64 `json:"up_to"`
	Color string  `json:"color"`
}

// StationTopology is the complete drawing of one station.
type StationTopology struct {
	StationID     string            `json:"station_id"`
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	Version       int64             `json:"version"` // maintained by the server
	UpdatedAtMs   int64             `json:"updated_at_ms,omitempty"`
	UpdatedBy     string            `json:"updated_by,omitempty"`
	GridBounds    GridBounds        `json:"grid_bounds"`
	Components    []EquipmentNode   `json:"components"`
	Pipelines     []PipelineSegment `json:"pipelines"`
	PressureBands []PressureBand    `json:"pressure_bands,omitempty"`
}

// Issue is one validation finding.
type Issue struct {
	Severity string `json:"severity"` // "error" | "warning"
	Path     string `json:"path"`     // e.g. components[3].bindings.primary_tag
	ID       string `json:"id,omitempty"`
	Message  string `json:"message"`
}

// HasErrors reports whether any issue is an error.
func HasErrors(issues []Issue) bool { return ErrorCount(issues) > 0 }

// ErrorCount returns the number of errors among issues.
func ErrorCount(issues []Issue) int {
	n := 0
	for _, i := range issues {
		if i.Severity == "error" {
			n++
		}
	}
	return n
}

// Normalize trims text fields and replaces nil slices by empty ones so the
// document always serialises to the same shape.
func (t *StationTopology) Normalize() {
	t.StationID = strings.TrimSpace(t.StationID)
	t.Name = strings.TrimSpace(t.Name)
	t.Description = strings.TrimSpace(t.Description)
	if t.Components == nil {
		t.Components = []EquipmentNode{}
	}
	if t.Pipelines == nil {
		t.Pipelines = []PipelineSegment{}
	}
	for i := range t.Components {
		c := &t.Components[i]
		c.ID = strings.TrimSpace(c.ID)
		c.Bindings.PrimaryTag = strings.TrimSpace(c.Bindings.PrimaryTag)
		c.Bindings.StateTag = strings.TrimSpace(c.Bindings.StateTag)
	}
	for i := range t.Pipelines {
		p := &t.Pipelines[i]
		p.ID = strings.TrimSpace(p.ID)
		p.BoundPressureTag = strings.TrimSpace(p.BoundPressureTag)
		if p.PathPoints == nil {
			p.PathPoints = []Point{}
		}
	}
}

// BoundTags returns every SCADA tag referenced by the topology, sorted and unique.
func (t *StationTopology) BoundTags() []string {
	set := map[string]struct{}{}
	add := func(s string) {
		if s != "" {
			set[s] = struct{}{}
		}
	}
	for _, c := range t.Components {
		add(c.Bindings.PrimaryTag)
		add(c.Bindings.StateTag)
	}
	for _, p := range t.Pipelines {
		add(p.BoundPressureTag)
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

type validator struct{ issues []Issue }

func (v *validator) err(path, id, format string, a ...any) {
	v.issues = append(v.issues, Issue{Severity: "error", Path: path, ID: id, Message: fmt.Sprintf(format, a...)})
}

func (v *validator) warn(path, id, format string, a ...any) {
	v.issues = append(v.issues, Issue{Severity: "warning", Path: path, ID: id, Message: fmt.Sprintf(format, a...)})
}

func validateTag(v *validator, path, id, tag string) {
	if tag != "" && !tagPattern.MatchString(tag) {
		v.err(path, id, "invalid tag id %q (allowed: letters, digits, _ . : -; max 128)", tag)
	}
}

func validateColorRules(v *validator, path, id string, rules []ColorRule) {
	if len(rules) > MaxColorRules {
		v.err(path, id, "at most %d color rules per element", MaxColorRules)
	}
	for i, r := range rules {
		if !validOps[r.Op] {
			v.err(fmt.Sprintf("%s[%d].op", path, i), id, "unknown operator %q", r.Op)
		}
		if !colorPattern.MatchString(r.Color) {
			v.err(fmt.Sprintf("%s[%d].color", path, i), id, "color must look like #rrggbb, got %q", r.Color)
		}
	}
}

func isInstrument(t string) bool {
	return t == TypePressureTx || t == TypeTemperatureTx || t == TypePressureGauge
}

// Validate checks the topology and returns all findings (errors block saving
// and deploying, warnings do not). It never mutates t.
func (t *StationTopology) Validate() []Issue {
	v := &validator{}
	if !stationPattern.MatchString(t.StationID) {
		v.err("station_id", "", "station_id must be 1-64 characters of letters, digits, _ or -")
	}
	if t.Name == "" || len(t.Name) > 128 {
		v.err("name", "", "name is required (max 128 characters)")
	}
	if len(t.Description) > 1000 {
		v.err("description", "", "description too long (max 1000 characters)")
	}
	gb := t.GridBounds
	if gb.Width < minCanvas || gb.Width > maxCanvas || gb.Height < minCanvas || gb.Height > maxCanvas {
		v.err("grid_bounds", "", "width and height must be %d..%d", minCanvas, maxCanvas)
	}
	if gb.GridSize < minGrid || gb.GridSize > maxGrid {
		v.err("grid_bounds.grid_size", "", "grid_size must be %d..%d", minGrid, maxGrid)
	}
	if len(t.Components) > MaxComponents {
		v.err("components", "", "too many components (max %d)", MaxComponents)
	}
	if len(t.Pipelines) > MaxPipelines {
		v.err("pipelines", "", "too many pipelines (max %d)", MaxPipelines)
	}
	if len(t.PressureBands) > MaxBands {
		v.err("pressure_bands", "", "at most %d pressure bands", MaxBands)
	}
	for i, b := range t.PressureBands {
		if !colorPattern.MatchString(b.Color) {
			v.err(fmt.Sprintf("pressure_bands[%d].color", i), "", "color must look like #rrggbb")
		}
		if i > 0 && b.UpTo <= t.PressureBands[i-1].UpTo {
			v.err(fmt.Sprintf("pressure_bands[%d].up_to", i), "", "bands must be in strictly ascending order")
		}
	}
	if len(t.Components) > MaxComponents || len(t.Pipelines) > MaxPipelines {
		return v.issues // do not walk oversized documents
	}

	ids := map[string]string{} // id -> "component" | "pipeline"
	nodes := map[string]*EquipmentNode{}
	positions := map[[2]int]string{}
	for i := range t.Components {
		c := &t.Components[i]
		p := fmt.Sprintf("components[%d]", i)
		switch {
		case !idPattern.MatchString(c.ID):
			v.err(p+".id", c.ID, "id must be 1-64 characters of letters, digits, _ . -")
		case ids[c.ID] != "":
			v.err(p+".id", c.ID, "duplicate id %q", c.ID)
		default:
			ids[c.ID] = "component"
			nodes[c.ID] = c
		}
		if !IsKnownType(c.Type) {
			v.err(p+".type", c.ID, "unknown equipment type %q", c.Type)
		}
		if c.Rotation != 0 && c.Rotation != 90 && c.Rotation != 180 && c.Rotation != 270 {
			v.err(p+".rotation", c.ID, "rotation must be 0, 90, 180 or 270")
		}
		if c.X < 0 || c.Y < 0 || c.X > gb.Width || c.Y > gb.Height {
			v.err(p, c.ID, "position (%d,%d) is outside the %dx%d canvas", c.X, c.Y, gb.Width, gb.Height)
		}
		if other, dup := positions[[2]int{c.X, c.Y}]; dup {
			v.warn(p, c.ID, "%s is placed exactly on top of %s", c.ID, other)
		} else {
			positions[[2]int{c.X, c.Y}] = c.ID
		}
		validateTag(v, p+".bindings.primary_tag", c.ID, c.Bindings.PrimaryTag)
		validateTag(v, p+".bindings.state_tag", c.ID, c.Bindings.StateTag)
		d := c.Display
		if len(d.Label) > 64 || len(d.LabelPrefix) > 32 || len(d.Unit) > 16 {
			v.err(p+".display", c.ID, "label (64), label_prefix (32) or unit (16) too long")
		}
		if d.Decimals != nil && (*d.Decimals < 0 || *d.Decimals > 6) {
			v.err(p+".display.decimals", c.ID, "decimals must be 0..6")
		}
		validateColorRules(v, p+".display.color_rules", c.ID, d.ColorRules)
		if isInstrument(c.Type) && c.Bindings.PrimaryTag == "" {
			v.warn(p+".bindings.primary_tag", c.ID, "%s has no primary tag bound", c.ID)
		}
		if (c.Type == TypeBallValve || c.Type == TypeSlamShutValve) && c.Bindings.StateTag == "" {
			v.warn(p+".bindings.state_tag", c.ID, "%s has no state tag bound, so its position cannot be shown live", c.ID)
		}
	}

	used := map[string]string{} // "node/terminal" -> pipe id
	connected := map[string]bool{}
	for i := range t.Pipelines {
		pl := &t.Pipelines[i]
		p := fmt.Sprintf("pipelines[%d]", i)
		switch {
		case !idPattern.MatchString(pl.ID):
			v.err(p+".id", pl.ID, "id must be 1-64 characters of letters, digits, _ . -")
		case ids[pl.ID] != "":
			v.err(p+".id", pl.ID, "duplicate id %q", pl.ID)
		default:
			ids[pl.ID] = "pipeline"
		}
		if pl.FromNodeID == pl.ToNodeID && pl.FromNodeID != "" {
			v.err(p, pl.ID, "pipe connects %s to itself", pl.FromNodeID)
		}
		ends := []struct{ node, term, field string }{
			{pl.FromNodeID, pl.FromTerminal, "from"}, {pl.ToNodeID, pl.ToTerminal, "to"},
		}
		for _, e := range ends {
			n := nodes[e.node]
			if n == nil {
				v.err(p+"."+e.field+"_node_id", pl.ID, "%s node %q does not exist", e.field, e.node)
				continue
			}
			okTerm := false
			for _, tn := range terminalsByType[n.Type] {
				if tn == e.term {
					okTerm = true
				}
			}
			if !okTerm {
				v.err(p+"."+e.field+"_terminal", pl.ID, "%s has no terminal %q (valid: %s)", n.ID, e.term, strings.Join(terminalsByType[n.Type], ", "))
				continue
			}
			key := e.node + "/" + e.term
			if prev, dup := used[key]; dup {
				v.err(p+"."+e.field+"_terminal", pl.ID, "terminal %s is already used by pipe %s", key, prev)
			} else {
				used[key] = pl.ID
			}
			connected[e.node] = true
		}
		if len(pl.PathPoints) > MaxPathPoints {
			v.err(p+".path_points", pl.ID, "too many bend points (max %d)", MaxPathPoints)
		}
		for j, pt := range pl.PathPoints {
			if pt.X < 0 || pt.Y < 0 || pt.X > gb.Width || pt.Y > gb.Height {
				v.err(fmt.Sprintf("%s.path_points[%d]", p, j), pl.ID, "point (%d,%d) is outside the canvas", pt.X, pt.Y)
			}
		}
		if !diameterPattern.MatchString(pl.NominalDiameter) {
			v.err(p+".nominal_diameter", pl.ID, "nominal_diameter must look like \"8-inch\" or \"DN200\", got %q", pl.NominalDiameter)
		}
		validateTag(v, p+".bound_pressure_tag", pl.ID, pl.BoundPressureTag)
		if pl.BoundPressureTag == "" {
			v.warn(p+".bound_pressure_tag", pl.ID, "pipe %s has no pressure tag, it will not change colour with pressure", pl.ID)
		}
	}
	for i := range t.Components {
		c := &t.Components[i]
		if !connected[c.ID] && c.Type != "" {
			v.warn(fmt.Sprintf("components[%d]", i), c.ID, "%s is not connected to any pipe", c.ID)
		}
	}
	return v.issues
}
