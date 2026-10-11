package model

// DefaultPressureBands colours pipes by pressure in PSI.
func DefaultPressureBands() []PressureBand {
	return []PressureBand{
		{UpTo: 10, Color: "#6e7681"},
		{UpTo: 100, Color: "#3fb950"},
		{UpTo: 200, Color: "#d29922"},
		{UpTo: 1e9, Color: "#f0883e"},
	}
}

// ExampleStation returns a complete city gate station drawing whose tags
// follow the simulator's naming (<station>_P_IN, _P_OUT, _TEMP, _INLET_VLV,
// _SLAM_SHUT, _OUTLET_VLV, _P_OUT_SP). It is valid and ready to save.
//
// Layout (flow left to right, 20 px grid):
//
//	FL_IN - BV_IN - FLT_01 - J_IN - J_T - SSV_01 - REG_01 - J_OUT - BV_OUT - FL_OUT
//	                          |      |                        |  \
//	                        PT_IN  TT_IN                    PT_OUT ODR_01
func ExampleStation(stationID, name string) *StationTopology {
	s := stationID
	dec1 := 1
	node := func(id, typ string, x, y, rot int, primary, state string, d DisplaySettings) EquipmentNode {
		return EquipmentNode{ID: id, Type: typ, X: x, Y: y, Rotation: rot,
			Bindings: TagBindings{PrimaryTag: primary, StateTag: state}, Display: d}
	}
	pipe := func(id, fn, ft, tn, tt, dia, tag string) PipelineSegment {
		return PipelineSegment{ID: id, FromNodeID: fn, FromTerminal: ft, ToNodeID: tn, ToTerminal: tt,
			PathPoints: []Point{}, NominalDiameter: dia, BoundPressureTag: tag}
	}
	return &StationTopology{
		StationID:  stationID,
		Name:       name,
		GridBounds: GridBounds{Width: 1200, Height: 600, GridSize: 20},
		Components: []EquipmentNode{
			node("FL_IN", TypeFlange, 60, 300, 0, "", "", DisplaySettings{Label: "INLET"}),
			node("BV_IN", TypeBallValve, 140, 300, 0, "", s+"_INLET_VLV", DisplaySettings{Label: "Inlet valve"}),
			node("FLT_01", TypeFilter, 260, 300, 0, "", "", DisplaySettings{Label: "Filter"}),
			node("J_IN", TypeJunction, 380, 300, 0, "", "", DisplaySettings{}),
			node("PT_IN", TypePressureTx, 380, 200, 0, s+"_P_IN", "", DisplaySettings{
				LabelPrefix: "P-IN", Decimals: &dec1,
				ColorRules: []ColorRule{{Op: ">=", Value: 251, Color: "#f85149"}, {Op: ">=", Value: 250.5, Color: "#f0883e"}},
			}),
			node("J_T", TypeJunction, 460, 300, 0, "", "", DisplaySettings{}),
			node("TT_IN", TypeTemperatureTx, 460, 200, 0, s+"_TEMP", "", DisplaySettings{LabelPrefix: "T", Decimals: &dec1}),
			node("SSV_01", TypeSlamShutValve, 540, 300, 0, "", s+"_SLAM_SHUT", DisplaySettings{Label: "Slam-shut"}),
			node("REG_01", TypeRegulator, 680, 300, 0, s+"_P_OUT_SP", "", DisplaySettings{Label: "Regulator", LabelPrefix: "SP", Decimals: &dec1}),
			node("J_OUT", TypeJunction, 800, 300, 0, "", "", DisplaySettings{}),
			node("PT_OUT", TypePressureTx, 800, 200, 0, s+"_P_OUT", "", DisplaySettings{LabelPrefix: "P-OUT", Decimals: &dec1}),
			node("ODR_01", TypeOdorizer, 800, 380, 90, "", "", DisplaySettings{Label: "Odorizer"}),
			node("BV_OUT", TypeBallValve, 900, 300, 0, "", s+"_OUTLET_VLV", DisplaySettings{Label: "Outlet valve"}),
			node("FL_OUT", TypeFlange, 1000, 300, 180, "", "", DisplaySettings{Label: "OUTLET"}),
		},
		Pipelines: []PipelineSegment{
			pipe("PIPE_01", "FL_IN", "port", "BV_IN", "in", "8-inch", s+"_P_IN"),
			pipe("PIPE_02", "BV_IN", "out", "FLT_01", "in", "8-inch", s+"_P_IN"),
			pipe("PIPE_03", "FLT_01", "out", "J_IN", "w", "8-inch", s+"_P_IN"),
			pipe("PIPE_04", "J_IN", "e", "J_T", "w", "8-inch", s+"_P_IN"),
			pipe("PIPE_05", "PT_IN", "port", "J_IN", "n", "2-inch", s+"_P_IN"),
			pipe("PIPE_06", "TT_IN", "port", "J_T", "n", "2-inch", s+"_P_IN"),
			pipe("PIPE_07", "J_T", "e", "SSV_01", "in", "8-inch", s+"_P_IN"),
			pipe("PIPE_08", "SSV_01", "out", "REG_01", "in", "8-inch", s+"_P_IN"),
			pipe("PIPE_09", "REG_01", "out", "J_OUT", "w", "12-inch", s+"_P_OUT"),
			pipe("PIPE_10", "PT_OUT", "port", "J_OUT", "n", "2-inch", s+"_P_OUT"),
			pipe("PIPE_11", "J_OUT", "s", "ODR_01", "in", "2-inch", s+"_P_OUT"),
			pipe("PIPE_12", "J_OUT", "e", "BV_OUT", "in", "12-inch", s+"_P_OUT"),
			pipe("PIPE_13", "BV_OUT", "out", "FL_OUT", "port", "12-inch", s+"_P_OUT"),
		},
		PressureBands: DefaultPressureBands(),
	}
}
