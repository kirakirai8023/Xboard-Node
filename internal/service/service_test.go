// SPDX-License-Identifier: MPL-2.0

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kirakirai8023/Xboard-Node/internal/cert"
	"github.com/kirakirai8023/Xboard-Node/internal/config"
	"github.com/kirakirai8023/Xboard-Node/internal/controlplane"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel"
	"github.com/kirakirai8023/Xboard-Node/internal/limiter"
	"github.com/kirakirai8023/Xboard-Node/internal/model"
	"github.com/kirakirai8023/Xboard-Node/internal/tracker"
	"golang.org/x/time/rate"
)

func TestQueuedWSEvents_CoalescesUserChanges(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.wsEvents = make(chan controlplane.Event, 16)
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "old"}})
	s.wsEvents <- controlplane.Event{Type: controlplane.EventSyncUserDelta, DeltaAction: "add", DeltaUsers: []model.UserSpec{{ID: 2, UUID: "new"}}}
	s.wsEvents <- controlplane.Event{Type: controlplane.EventSyncUserDelta, DeltaAction: "add", DeltaUsers: []model.UserSpec{{ID: 3, UUID: "third"}}}
	s.handleQueuedWSEvents(context.Background(), controlplane.Event{Type: controlplane.EventSyncUsers, Users: []model.UserSpec{{ID: 1, UUID: "old"}}})
	if k.updateCalls != 1 || len(s.lastUsers) != 3 {
		t.Fatalf("updates=%d, users=%v", k.updateCalls, s.lastUsers)
	}
}

func TestQueuedWSEvents_RevocationBeforeReauthorization(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	s.wsEvents = make(chan controlplane.Event, 16)
	users := []model.UserSpec{{ID: 1, UUID: "old"}}
	s.updateUserState(users)
	s.wsEvents <- controlplane.Event{Type: controlplane.EventSyncUsers, Users: users}
	s.handleQueuedWSEvents(context.Background(), controlplane.Event{Type: controlplane.EventSyncUsers, Users: []model.UserSpec{}})
	if !k.running || k.startCalls != 1 {
		t.Fatal("revocation was skipped before the same user was authorized again")
	}
}

func TestQueuedWSEvents_EmptySnapshotRevokesUsers(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "old"}})
	s.handleQueuedWSEvents(context.Background(), controlplane.Event{Type: controlplane.EventSyncUsers, Users: []model.UserSpec{}})
	if k.running || len(s.lastUsers) != 0 {
		t.Fatal("empty snapshot did not revoke users")
	}
}

type fakeKernel struct {
	running bool

	startErr  error
	updateErr error
	addErr    error

	startCalls  int
	updateCalls int
	addCalls    int
	removeCalls int

	onUpdateUsers func([]model.UserSpec)
	onAddUsers    func([]model.UserSpec)
	onRemoveUsers func([]model.UserSpec)
	onStart       func()
	onStop        func()
	onReload      func(kernel.TLSCert) error
	traffic       map[int][2]int64

	speedLimitFunc  func(string) *rate.Limiter
	deviceLimitFunc func(string) (int, bool)
}

func (f *fakeKernel) Name() string                      { return "fake" }
func (f *fakeKernel) Protocols() []string               { return []string{"vless"} }
func (f *fakeKernel) Capabilities() kernel.Capabilities { return kernel.Capabilities{} }
func (f *fakeKernel) Start(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	_, _, _ = nodeConfig, users, tls
	f.startCalls++
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	if f.onStart != nil {
		f.onStart()
	}
	return nil
}
func (f *fakeKernel) Stop() {
	f.running = false
	if f.onStop != nil {
		f.onStop()
	}
}
func (f *fakeKernel) IsRunning() bool { return f.running }
func (f *fakeKernel) Reload(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	_, _, _ = nodeConfig, users, tls
	if f.onReload != nil {
		return f.onReload(tls)
	}
	return nil
}
func (f *fakeKernel) AddUsers(users []model.UserSpec) (int, error) {
	f.addCalls++
	if f.onAddUsers != nil {
		f.onAddUsers(users)
	}
	if f.addErr != nil {
		return 0, f.addErr
	}
	return len(users), nil
}
func (f *fakeKernel) RemoveUsers(users []model.UserSpec) (int, error) {
	f.removeCalls++
	if f.onRemoveUsers != nil {
		f.onRemoveUsers(users)
	}
	return len(users), nil
}
func (f *fakeKernel) UpdateUsers(users []model.UserSpec) (int, int, error) {
	f.updateCalls++
	if f.onUpdateUsers != nil {
		f.onUpdateUsers(users)
	}
	if f.updateErr != nil {
		return 0, 0, f.updateErr
	}
	return len(users), 0, nil
}
func (f *fakeKernel) GetUserTraffic(ctx context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	_ = ctx
	return f.traffic, nil, 0, nil
}
func (f *fakeKernel) CloseConnection(ctx context.Context, connID string) error {
	_, _ = ctx, connID
	return nil
}
func (f *fakeKernel) CloseUserConnections(ctx context.Context, uuid string) error {
	_, _ = ctx, uuid
	return nil
}
func (f *fakeKernel) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) { f.speedLimitFunc = fn }
func (f *fakeKernel) SetDeviceLimitFunc(fn func(uuid string) (int, bool))  { f.deviceLimitFunc = fn }
func (f *fakeKernel) UpdateGlobalDevices(users map[int][]string)           { _ = users }
func (f *fakeKernel) ClearGlobalDevices()                                  {}

func newTestService(k *fakeKernel) *Service {
	sharedLimiter := limiter.New()
	s := &Service{
		cfg:          &config.Config{Kernel: config.KernelConfig{Type: "singbox"}},
		tracker:      tracker.New(),
		kernel:       k,
		limiter:      sharedLimiter,
		speedTracker: limiter.NewSpeedTracker(sharedLimiter),
		cert:         cert.NewManager(config.CertConfig{}),
	}
	k.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	k.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)
	return s
}

type testControlPlane struct {
	controlplane.ControlPlane
	poll          func(context.Context) (controlplane.Snapshot, error)
	report        func(controlplane.ReportPayload) error
	invalidations int
	resyncNeeded  bool
}

func (p *testControlPlane) SupportsPolling() bool            { return true }
func (p *testControlPlane) SupportsReporting() bool          { return true }
func (p *testControlPlane) Metrics() controlplane.APIMetrics { return controlplane.APIMetrics{} }
func (p *testControlPlane) InvalidateSnapshot()              { p.invalidations++ }
func (p *testControlPlane) ConsumeResyncRequest() bool {
	needed := p.resyncNeeded
	p.resyncNeeded = false
	return needed
}
func (p *testControlPlane) Poll(ctx context.Context) (controlplane.Snapshot, error) {
	return p.poll(ctx)
}
func (p *testControlPlane) Report(payload controlplane.ReportPayload) error { return p.report(payload) }
func (p *testControlPlane) Initial(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	return controlplane.Bootstrap{Config: &model.NodeSpec{Protocol: "vless", ServerPort: 443}, Users: []model.UserSpec{{ID: 1, UUID: "u"}}}, nil
}

func awaitPull(t *testing.T, s *Service) pullResult {
	t.Helper()
	select {
	case result := <-s.pullResults:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("poll did not return")
		return pullResult{}
	}
}

func TestReconciliation_RetriesFailureWhileConnected(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failed=", fail), func(t *testing.T) {
			calls := 0
			cp := &testControlPlane{poll: func(context.Context) (controlplane.Snapshot, error) {
				calls++
				if fail && calls == 1 {
					return controlplane.Snapshot{}, errors.New("temporary failure")
				}
				return controlplane.Snapshot{}, nil
			}}
			s := newTestService(&fakeKernel{})
			s.source, s.pullResults = cp, make(chan pullResult, 1)
			s.requestWSResync(context.Background(), "drop")
			first := awaitPull(t, s)
			// A second dropped event arrived after the request started.
			s.requestWSResync(context.Background(), "another_drop")
			s.applyPullResult(context.Background(), first)
			if !s.wsResyncPending.Load() {
				t.Fatal("unfinished reconciliation was forgotten")
			}
			// The timer keeps retrying pending work, including its backoff tick.
			for i := 0; i < 3 && s.wsResyncPending.Load(); i++ {
				s.pullViaAPIAsync(context.Background())
				if s.pullActive.Load() {
					s.applyPullResult(context.Background(), awaitPull(t, s))
				}
			}
			if calls != 2 || s.wsResyncPending.Load() {
				t.Fatalf("calls=%d, pending=%v", calls, s.wsResyncPending.Load())
			}
		})
	}
}

func TestReconciliation_StaleRESTCannotReauthorizeUser(t *testing.T) {
	for _, coalesced := range []bool{false, true} {
		t.Run(fmt.Sprint("coalesced=", coalesced), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			users := []model.UserSpec{{ID: 1, UUID: "u"}}
			cp := &testControlPlane{poll: func(context.Context) (controlplane.Snapshot, error) {
				close(started)
				<-release
				return controlplane.Snapshot{Users: users}, nil
			}}
			k := &fakeKernel{running: true}
			s := newTestService(k)
			s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
			s.source, s.pullResults = cp, make(chan pullResult, 1)
			s.wsEvents = make(chan controlplane.Event, 16)
			s.updateUserState(users)
			s.pullViaAPIAsync(context.Background())
			<-started
			s.handleWSEvent(context.Background(), controlplane.Event{Type: controlplane.EventSyncUsers, Users: []model.UserSpec{}})
			if coalesced {
				s.handleQueuedWSEvents(context.Background(), controlplane.Event{Type: controlplane.EventSyncUserDelta, DeltaAction: "add", DeltaUsers: []model.UserSpec{{ID: 2, UUID: "other"}}})
			}
			close(release)
			s.applyPullResult(context.Background(), awaitPull(t, s))
			for _, user := range s.lastUsers {
				if user.ID == 1 {
					t.Fatal("stale REST snapshot reauthorized revoked user")
				}
			}
			if !s.wsResyncPending.Load() || cp.invalidations == 0 {
				t.Fatal("stale response did not schedule a full reconciliation")
			}
		})
	}
}

func TestReconciliation_StaleConfigCannotReplacePush(t *testing.T) {
	s := newTestService(&fakeKernel{})
	old := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	newConfig := &model.NodeSpec{Protocol: "vless", ServerPort: 8443}
	s.lastConfig, s.lastConfigHash = old, computeConfigHash(old)
	result := pullResult{config: old, configHash: computeConfigHash(old)}
	s.handleWSEvent(context.Background(), controlplane.Event{Type: controlplane.EventSyncConfig, Config: newConfig})
	s.applyPullResult(context.Background(), result)
	if s.lastConfig.ServerPort != 8443 {
		t.Fatal("stale response replaced pushed config")
	}
}

func TestReconciliation_DropInvalidatesInflightSnapshot(t *testing.T) {
	s := newTestService(&fakeKernel{})
	s.source = &testControlPlane{}
	s.updateUserState([]model.UserSpec{{ID: 2, UUID: "current"}})
	s.resyncRevision = 1
	s.applyPullResult(context.Background(), pullResult{
		users: []model.UserSpec{{ID: 1, UUID: "revoked"}}, userHash: "old",
	})
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 2 || !s.wsResyncPending.Load() {
		t.Fatal("a response started before the dropped event must not change authorization")
	}
}

func TestReconciliation_LostNotificationDiscardsInflightSnapshot(t *testing.T) {
	s := newTestService(&fakeKernel{})
	s.source = &testControlPlane{resyncNeeded: true}
	s.pullActive.Store(true)
	s.updateUserState([]model.UserSpec{{ID: 2, UUID: "current"}})
	s.applyPullResult(context.Background(), pullResult{
		users: []model.UserSpec{{ID: 1, UUID: "revoked"}}, userHash: "old",
	})
	if len(s.lastUsers) != 1 || s.lastUsers[0].ID != 2 || !s.wsResyncPending.Load() || s.resyncRevision != 1 {
		t.Fatal("a lost queue notification must invalidate the in-flight authorization snapshot")
	}
}

func TestCertificateRefresh_IndependentOfPanelAndRetriesReload(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "u"}})
	s.cert = cert.NewManager(config.CertConfig{CertMode: "self", CertDir: t.TempDir(), Domain: "test.invalid"})
	if err := s.cert.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	k.onReload = func(got kernel.TLSCert) error {
		calls++
		if len(got.CertPEM) == 0 {
			t.Fatal("missing new certificate")
		}
		if calls == 1 {
			return errors.New("temporary reload failure")
		}
		return nil
	}
	k.startErr = errors.New("temporary start failure")
	s.refreshCertificate(context.Background())
	s.refreshCertificate(context.Background())
	s.refreshCertificate(context.Background())
	if calls != 2 {
		t.Fatalf("reloads=%d; want failed attempt, retry, then no-op", calls)
	}
}

func TestShutdown_ReportsFinalKernelCounters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	k := &fakeKernel{onStart: cancel}
	k.onStop = func() { k.traffic = map[int][2]int64{1: {1000, 2000}} }
	var report controlplane.ReportPayload
	cp := &testControlPlane{report: func(p controlplane.ReportPayload) error { report = p; return nil }}
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Node: config.NodeConfig{TrackInterval: 3600, DeviceReportInterval: 3600}, WS: config.WSConfig{DiscoveryInterval: 3600}}
	s := newService(cfg, cp)
	s.kernel = k
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if report.Traffic[1] != [2]int64{1000, 2000} {
		t.Fatalf("final traffic=%v", report.Traffic)
	}
}

func TestShutdown_WaitsForFailedInflightReport(t *testing.T) {
	s := newTestService(&fakeKernel{})
	started, release := make(chan struct{}), make(chan struct{})
	cp := &testControlPlane{report: func(controlplane.ReportPayload) error {
		close(started)
		<-release
		return errors.New("temporary failure")
	}}
	s.source, s.sink = cp, cp
	s.tracker.Process(map[int][2]int64{1: {100, 200}}, nil, 0)
	s.pushReportAsync()
	<-started
	s.tracker.Process(map[int][2]int64{1: {150, 275}}, nil, 0)
	close(release)
	s.pushWG.Wait()
	if got := s.tracker.FlushTraffic()[1]; got != [2]int64{150, 275} {
		t.Fatalf("restored final traffic=%v", got)
	}
}

func TestApplyUserUpdatePreparesLimiterBeforeKernelUpdate(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	k.onUpdateUsers = func(users []model.UserSpec) {
		if len(users) != 1 || users[0].UUID != "uuid-new" {
			t.Fatalf("unexpected users passed to UpdateUsers: %#v", users)
		}
		if got := k.speedLimitFunc("uuid-new"); got == nil {
			t.Fatal("expected new user's limiter to be visible before kernel UpdateUsers")
		}
	}

	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if got := k.updateCalls; got != 1 {
		t.Fatalf("UpdateUsers call count = %d, want 1", got)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-new" {
		t.Fatalf("lastUsers = %#v, want new users", s.lastUsers)
	}
	if s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("expected limiter for new user after successful update")
	}
}

func TestApplyUserUpdateRestoresStateWhenKernelAndRestartFail(t *testing.T) {
	k := &fakeKernel{
		running:   true,
		updateErr: errors.New("update failed"),
		startErr:  errors.New("restart failed"),
	}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)
	oldHash := s.lastUserHash

	newUsers := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	s.applyUserUpdate(context.Background(), newUsers, computeUserHash(newUsers))

	if got := k.startCalls; got != 1 {
		t.Fatalf("Start call count = %d, want 1", got)
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != "uuid-old" {
		t.Fatalf("lastUsers = %#v, want restored old users", s.lastUsers)
	}
	if s.lastUserHash != oldHash {
		t.Fatalf("lastUserHash = %q, want %q", s.lastUserHash, oldHash)
	}
	if s.speedTracker.GetLimiter("uuid-old") == nil {
		t.Fatal("expected old limiter to be restored after rollback")
	}
	if s.speedTracker.GetLimiter("uuid-new") != nil {
		t.Fatal("expected new limiter to be removed after rollback")
	}
}

func TestApplyUserDeltaAddPreparesLimiterBeforeKernelUpdate(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	oldUsers := []model.UserSpec{{ID: 1, UUID: "uuid-old", SpeedLimit: 4}}
	s.updateUserState(oldUsers)

	delta := []model.UserSpec{{ID: 2, UUID: "uuid-new", SpeedLimit: 8}}
	k.onAddUsers = func(users []model.UserSpec) {
		if len(users) != 1 || users[0].UUID != "uuid-new" {
			t.Fatalf("unexpected users passed to AddUsers: %#v", users)
		}
		if got := k.speedLimitFunc("uuid-new"); got == nil {
			t.Fatal("expected delta user's limiter to be visible before kernel AddUsers")
		}
	}

	s.applyUserDelta(context.Background(), "add", delta)

	if got := k.addCalls; got != 1 {
		t.Fatalf("AddUsers call count = %d, want 1", got)
	}
	if s.speedTracker.GetLimiter("uuid-new") == nil {
		t.Fatal("expected limiter for delta-added user after successful update")
	}
}

func TestUserRevocationAndRecovery(t *testing.T) {
	for _, snapshot := range []bool{true, false} {
		t.Run(fmt.Sprintf("snapshot=%t", snapshot), func(t *testing.T) {
			k := &fakeKernel{running: true}
			s := newTestService(k)
			s.lastConfig = &model.NodeSpec{Protocol: "vless"}
			oldUsers := []model.UserSpec{{ID: 1, UUID: "old-user", SpeedLimit: 4}}
			s.updateUserState(oldUsers)
			if snapshot {
				s.applyUserUpdate(context.Background(), []model.UserSpec{}, "")
			} else {
				s.applyUserDelta(context.Background(), "remove", oldUsers)
			}
			if k.running || len(s.lastUsers) != 0 || s.speedTracker.GetLimiter("old-user") != nil {
				t.Fatal("revoking all users must stop listeners and remove user state")
			}
			if k.updateCalls != 0 || k.startCalls != 0 {
				t.Fatal("must not pass empty users to kernel APIs that may enable anonymous access")
			}
			newUsers := []model.UserSpec{{ID: 2, UUID: "new-user", SpeedLimit: 8}}
			if snapshot {
				s.applyUserUpdate(context.Background(), newUsers, "")
			} else {
				s.applyUserDelta(context.Background(), "add", newUsers)
			}
			if !k.running || k.startCalls != 1 || len(s.lastUsers) != 1 || s.lastUsers[0].ID != 2 || s.speedTracker.GetLimiter("new-user") == nil {
				t.Fatal("new users must restart the stopped kernel with updated limits")
			}
		})
	}
}

func TestApplyUserUpdateBeforeConfig(t *testing.T) {
	k := &fakeKernel{}
	s := newTestService(k)
	users := []model.UserSpec{{ID: 1, UUID: "new-user"}}
	s.applyUserUpdate(context.Background(), users, "")
	if k.startCalls != 0 || len(s.lastUsers) != 1 {
		t.Fatal("retain users until a node config is available")
	}
}

func TestApplyUserDeltaCredentialRotation(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless"}
	s.updateUserState([]model.UserSpec{{ID: 1, UUID: "old-key", SpeedLimit: 4}})
	newUsers := []model.UserSpec{{ID: 1, UUID: "new-key", SpeedLimit: 8}}
	k.onUpdateUsers = func(users []model.UserSpec) {
		if len(users) != 1 || users[0].UUID != "new-key" || k.speedLimitFunc("new-key") == nil {
			t.Fatal("replacement credentials and limits must be applied together")
		}
	}
	s.applyUserDelta(context.Background(), "add", newUsers)
	if k.updateCalls != 1 || k.removeCalls != 0 || k.addCalls != 0 || s.lastUsers[0].UUID != "new-key" {
		t.Fatal("credential rotation must not remove the sole user before installing its replacement")
	}
}

func TestValidateNodeRuntimeRejectsUnsupportedDNSProvider(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"http"}, &model.NodeSpec{
		Protocol: "http",
		CertConfig: &config.CertConfig{
			CertMode:    "dns",
			DNSProvider: "3123123",
			Domain:      "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() == "" {
		t.Fatal("expected non-empty error")
	}
	if got := err.Error(); !strings.HasPrefix(got, `unsupported cert_config.dns_provider "3123123" (supported: `) {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeAllowsSelfManagedTLSBeforeFilesExist(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"anytls", "hysteria"}, &model.NodeSpec{
		Protocol: "anytls",
		CertConfig: &config.CertConfig{
			CertMode: "self",
			Domain:   "example.com",
		},
	}, kernel.TLSCert{})
	if err != nil {
		t.Fatalf("expected self-managed TLS config to pass validation, got %v", err)
	}
}

func TestValidateNodeRuntimeAllowsSingboxRealityWithRequiredFields(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"private_key": "test-key",
			"server_name": "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err != nil {
		t.Fatalf("expected sing-box reality validation to pass, got %v", err)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutTLSSettings(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutPrivateKey(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"server_name": "example.com",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings.private_key" {
		t.Fatalf("unexpected error: %v", got)
	}
}

func TestValidateNodeRuntimeRejectsRealityWithoutServerNameOrDest(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}}
	err := validateNodeRuntime(cfg, []string{"vless"}, &model.NodeSpec{
		Protocol: "vless",
		TLS:      2,
		TLSSettings: map[string]any{
			"private_key": "test-key",
		},
	}, kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "reality tls requires tls_settings.server_name or tls_settings.dest" {
		t.Fatalf("unexpected error: %v", got)
	}
}
