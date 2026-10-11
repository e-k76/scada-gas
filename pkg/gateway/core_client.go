package gateway

import (
	"context"

	"google.golang.org/grpc"

	scadav1 "github.com/example/scada-gas/gen/scada/v1"
)

// Core is the subset of the core server the gateway needs. It is an
// interface so the HTTP layer can be tested without a running core.
type Core interface {
	CurrentTelemetry(ctx context.Context) ([]*scadav1.TagSnapshot, error)
	ActiveAlarms(ctx context.Context, tagID string, minSeverity scadav1.AlarmSeverity) ([]*scadav1.AlarmEvent, error)
	Acknowledge(ctx context.Context, alarmID, user string) (*scadav1.AckAlarmResponse, error)
}

// GRPCCore implements Core over a gRPC connection.
type GRPCCore struct {
	data   scadav1.ScadaDataServiceClient
	alarms scadav1.ScadaAlarmServiceClient
	config scadav1.ScadaConfigServiceClient
}

// NewGRPCCore wraps an established (or lazily connecting) client connection.
func NewGRPCCore(conn grpc.ClientConnInterface) *GRPCCore {
	return &GRPCCore{
		data:   scadav1.NewScadaDataServiceClient(conn),
		alarms: scadav1.NewScadaAlarmServiceClient(conn),
		config: scadav1.NewScadaConfigServiceClient(conn),
	}
}

func (c *GRPCCore) CurrentTelemetry(ctx context.Context) ([]*scadav1.TagSnapshot, error) {
	r, err := c.data.GetCurrentTelemetry(ctx, &scadav1.GetCurrentTelemetryRequest{})
	if err != nil {
		return nil, err
	}
	return r.GetTags(), nil
}

func (c *GRPCCore) ActiveAlarms(ctx context.Context, tagID string, minSeverity scadav1.AlarmSeverity) ([]*scadav1.AlarmEvent, error) {
	r, err := c.alarms.GetActiveAlarms(ctx, &scadav1.GetActiveAlarmsRequest{TagId: tagID, MinSeverity: minSeverity})
	if err != nil {
		return nil, err
	}
	return r.GetAlarms(), nil
}

func (c *GRPCCore) Acknowledge(ctx context.Context, alarmID, user string) (*scadav1.AckAlarmResponse, error) {
	return c.alarms.AcknowledgeAlarm(ctx, &scadav1.AckAlarmRequest{AlarmId: alarmID, User: user})
}

// DeployTopology publishes a topology's runtime configuration to the core.
func (c *GRPCCore) DeployTopology(ctx context.Context, req *scadav1.DeployTopologyRequest) (*scadav1.DeployTopologyResponse, error) {
	return c.config.DeployTopology(ctx, req)
}

// RuntimeConfigs returns the configurations the core currently holds.
func (c *GRPCCore) RuntimeConfigs(ctx context.Context) ([]*scadav1.RuntimeConfig, error) {
	r, err := c.config.GetRuntimeConfigs(ctx, &scadav1.GetRuntimeConfigsRequest{})
	if err != nil {
		return nil, err
	}
	return r.GetConfigs(), nil
}
