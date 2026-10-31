package controlplane

import (
	"context"

	"github.com/kirakirai8023/Xboard-Node/internal/model"
)

type EventType string

const (
	EventSyncConfig    EventType = "sync.config"
	EventSyncUsers     EventType = "sync.users"
	EventSyncUserDelta EventType = "sync.user.delta"
	EventSyncDevices   EventType = "sync.devices"
)

type Event struct {
	Type        EventType
	Config      *model.NodeSpec
	Users       []model.UserSpec
	DeltaAction string
	DeltaUsers  []model.UserSpec
	DeviceUsers map[int][]string
}

type StatusChange struct {
	Connected   bool
	NeedsResync bool
}

type APIMetrics struct {
	Success uint64
	Failure uint64
}

type Bootstrap struct {
	PushInterval int
	PullInterval int
	Push         PushClient
	Config       *model.NodeSpec
	Users        []model.UserSpec
	Mailbox      *NodeMailbox
}

type Snapshot struct {
	Config *model.NodeSpec
	Users  []model.UserSpec
}

type ReportPayload struct {
	Traffic map[int][2]int64
	Alive   map[int][]string
	Online  map[int]int
	CPU     float64
	Mem     [2]uint64
	Swap    [2]uint64
	Disk    [2]uint64
	Metrics map[string]interface{}
}

type PushClient interface {
	Run(ctx context.Context)
	IsConnected() bool
	SendDeviceReport(devices map[int][]string)
}

type Source interface {
	Initial(ctx context.Context, metricsFn func() map[string]interface{}, events chan<- Event, statuses chan<- StatusChange) (Bootstrap, error)
	Poll(ctx context.Context) (Snapshot, error)
	Discover(ctx context.Context, metricsFn func() map[string]interface{}, events chan<- Event, statuses chan<- StatusChange) (PushClient, error)
	Metrics() APIMetrics
	SupportsPolling() bool
	SupportsDiscovery() bool
}

// SnapshotInvalidator requests an unconditional snapshot on the next poll.
// Implementations must allow invalidation while a poll is in flight.
type SnapshotInvalidator interface {
	InvalidateSnapshot()
}

// ReconciliationRequester retains event loss when the notification queue is
// full. The service consumes requests on its event loop and retries polling.
type ReconciliationRequester interface {
	ConsumeResyncRequest() bool
}

type Sink interface {
	Report(payload ReportPayload) error
	ReportDevices(push PushClient, devices map[int][]string)
	SupportsReporting() bool
	SupportsDeviceReports() bool
}

type ControlPlane interface {
	Source
	Sink
}
