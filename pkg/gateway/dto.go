// Package gateway implements the northbound HTTP/JSON API and the real-time
// WebSocket dispatcher in front of the SCADA core.
package gateway

import (
	"math"
	"strings"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

// TagJSON is the JSON form of a tag value (analog or digital).
type TagJSON struct {
	TagID       string   `json:"tag_id"`
	Type        string   `json:"type"` // "analog" | "digital"
	Value       *float64 `json:"value,omitempty"`
	RawValue    *float64 `json:"raw_value,omitempty"`
	State       *bool    `json:"state,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Quality     string   `json:"quality"` // GOOD, BAD_COMM_FAILURE, ...
	QualityCode int32    `json:"quality_code"`
	TimestampMs int64    `json:"timestamp_ms"`
	UpdateCount uint64   `json:"update_count,omitempty"`
	SOE         bool     `json:"soe,omitempty"`
	SourceFEP   string   `json:"source_fep,omitempty"`
	// LastChangeMs is when a digital tag last changed state (snapshots only).
	LastChangeMs int64 `json:"last_change_ms,omitempty"`
}

// AlarmJSON is the JSON form of an alarm event.
type AlarmJSON struct {
	AlarmID       string  `json:"alarm_id"`
	TagID         string  `json:"tag_id"`
	Severity      string  `json:"severity"` // INFO .. CRITICAL
	SeverityCode  int32   `json:"severity_code"`
	State         string  `json:"state"` // UNACK_ALARM, ACK_ALARM, UNACK_RTN, ACK_RTN
	StateCode     int32   `json:"state_code"`
	Active        bool    `json:"active"` // false once cleared (ACK_RTN)
	Message       string  `json:"message"`
	TriggerValue  float64 `json:"trigger_value"`
	TriggerTimeMs int64   `json:"trigger_time_ms"`
	AckTimeMs     int64   `json:"ack_time_ms,omitempty"`
	AckByUser     string  `json:"ack_by_user,omitempty"`
	RTNTimeMs     int64   `json:"rtn_time_ms,omitempty"`
}

// WebSocket message envelopes.
type snapshotMsg struct {
	Type   string      `json:"type"` // "snapshot"
	Time   int64       `json:"time"`
	Tags   []TagJSON   `json:"tags"`
	Alarms []AlarmJSON `json:"alarms"`
	Error  string      `json:"error,omitempty"`
}

type telemetryMsg struct {
	Type string    `json:"type"` // "telemetry"
	Time int64     `json:"time"`
	FEP  string    `json:"fep"`
	Seq  int64     `json:"seq"`
	Tags []TagJSON `json:"tags"`
}

type alarmMsg struct {
	Type  string    `json:"type"` // "alarm"
	Time  int64     `json:"time"`
	Alarm AlarmJSON `json:"alarm"`
}

// finite returns &v, or nil for NaN/Inf which JSON cannot represent.
func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

func qualityName(q scadav1.DataQuality) string {
	return strings.TrimPrefix(q.String(), "QUALITY_")
}

// TagFromSnapshot converts an RTDB snapshot entry.
func TagFromSnapshot(t *scadav1.TagSnapshot) TagJSON {
	j := TagJSON{
		TagID: t.GetTagId(), Quality: qualityName(t.GetQuality()), QualityCode: int32(t.GetQuality()),
		TimestampMs: t.GetTimestampUtcMs(), UpdateCount: t.GetUpdateCount(),
		SOE: t.GetIsSoe(), SourceFEP: t.GetSourceFepId(), Unit: t.GetEngineeringUnit(), LastChangeMs: t.GetLastChangeUtcMs(),
	}
	if t.GetIsDigital() {
		st := t.GetState()
		j.Type, j.State = "digital", &st
	} else {
		j.Type, j.Value, j.RawValue = "analog", finite(t.GetValue()), finite(t.GetRawValue())
	}
	return j
}

// TagFromAnalog converts a streamed analog update.
func TagFromAnalog(a *scadav1.AnalogTagValue, fep string) TagJSON {
	return TagJSON{
		TagID: a.GetTagId(), Type: "analog", Value: finite(a.GetValue()), RawValue: finite(a.GetRawValue()),
		Unit: a.GetEngineeringUnit(), Quality: qualityName(a.GetQuality()), QualityCode: int32(a.GetQuality()),
		TimestampMs: a.GetTimestampUtcMs(), SourceFEP: fep,
	}
}

// TagFromDigital converts a streamed digital update.
func TagFromDigital(d *scadav1.DigitalTagValue, fep string) TagJSON {
	st := d.GetState()
	return TagJSON{
		TagID: d.GetTagId(), Type: "digital", State: &st,
		Quality: qualityName(d.GetQuality()), QualityCode: int32(d.GetQuality()),
		TimestampMs: d.GetTimestampUtcMs(), SOE: d.GetIsSoe(), SourceFEP: fep,
	}
}

// AlarmFromEvent converts an alarm event.
func AlarmFromEvent(e *scadav1.AlarmEvent) AlarmJSON {
	return AlarmJSON{
		AlarmID: e.GetAlarmId(), TagID: e.GetTagId(),
		Severity:     strings.TrimPrefix(e.GetSeverity().String(), "SEVERITY_"),
		SeverityCode: int32(e.GetSeverity()),
		State:        strings.TrimPrefix(e.GetState().String(), "ALARM_"),
		StateCode:    int32(e.GetState()),
		Active:       e.GetState() != scadav1.AlarmState_ALARM_ACK_RTN,
		Message:      e.GetMessage(), TriggerValue: e.GetTriggerValue(),
		TriggerTimeMs: e.GetTriggerTimeUtcMs(), AckTimeMs: e.GetAckTimeUtcMs(),
		AckByUser: e.GetAckByUser(), RTNTimeMs: e.GetRtnTimeUtcMs(),
	}
}

func tagsFromSnapshots(in []*scadav1.TagSnapshot) []TagJSON {
	out := make([]TagJSON, 0, len(in))
	for _, t := range in {
		out = append(out, TagFromSnapshot(t))
	}
	return out
}

func alarmsFromEvents(in []*scadav1.AlarmEvent) []AlarmJSON {
	out := make([]AlarmJSON, 0, len(in))
	for _, e := range in {
		out = append(out, AlarmFromEvent(e))
	}
	return out
}
