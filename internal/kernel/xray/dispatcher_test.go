// SPDX-License-Identifier: MPL-2.0

package xray

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/kirakirai8023/Xboard-Node/internal/model"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"golang.org/x/time/rate"
)

type nativeDispatcher struct {
	routing.Dispatcher
	seen context.Context
	link *transport.Link
}

func (d *nativeDispatcher) Dispatch(ctx context.Context, _ xnet.Destination) (*transport.Link, error) {
	d.seen = ctx
	return d.link, nil
}
func (d *nativeDispatcher) DispatchLink(ctx context.Context, _ xnet.Destination, link *transport.Link) error {
	d.seen, d.link = ctx, link
	return nil
}

func TestLimitDispatcher_NativeVisionSession(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, nil, nil)
	original := &session.Inbound{User: &protocol.MemoryUser{Email: email}, Source: xnet.TCPDestination(xnet.ParseAddress("127.0.0.1"), 1234), CanSpliceCopy: 2}
	native := &nativeDispatcher{link: &transport.Link{Reader: nopReader{}, Writer: buf.Discard}}
	ld.innerDisp = native
	ctx, cancel := context.WithCancel(session.ContextWithInbound(context.Background(), original))
	defer cancel()
	link, err := ld.Dispatch(ctx, xnet.TCPDestination(xnet.ParseAddress("example.com"), 443))
	if err != nil {
		t.Fatal(err)
	}
	got := session.InboundFromContext(native.seen)
	if got != original || got.CanSpliceCopy != 2 {
		t.Fatal("native dispatch detached Vision's handshake state")
	}
	original.CanSpliceCopy = 1
	if got.CanSpliceCopy != 1 {
		t.Fatal("native outbound cannot observe Vision readiness")
	}
	if _, limited := link.Writer.(*bandwidthWriter); limited {
		t.Fatal("unlimited traffic has a bandwidth wrapper")
	}
	_ = link.Writer.(*closeTrackingWriter).Close()
	if ld.connCount.Load() != 0 {
		t.Fatal("native connection was not released")
	}
}

type closeSignalConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeSignalConn) Close() error { c.closed.Store(true); return nil }

func TestLimitDispatcher_LimitTransitionClosesOnlyNativeUser(t *testing.T) {
	ld := newTestDispatcher()
	ld.UpdateLimits(map[string]int{"user@1": 1, "user@2": 2}, nil, nil)
	sockets := []*closeSignalConn{{}, {}}
	for i, email := range []string{"user@1", "user@2"} {
		ctx, cancel := context.WithCancel(session.ContextWithInbound(context.Background(), &session.Inbound{Conn: sockets[i]}))
		link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
		ld.trackManagedLink(ctx, cancel, link, email, "127.0.0.1", false, true)
	}
	ld.UpdateSpeedLimits(map[string]*rate.Limiter{"user@1": rate.NewLimiter(1000, 1000), "user@2": nil})
	if !sockets[0].closed.Load() || sockets[1].closed.Load() {
		t.Fatal("limit transition interrupted the wrong user")
	}
	ld.CloseUser("user@2")
	if !sockets[1].closed.Load() || ld.connCount.Load() != 0 {
		t.Fatal("targeted close did not clean up native connections")
	}
}

func TestLimitDispatcher_CompatibleDisablesSplice(t *testing.T) {
	ld := newTestDispatcher()
	ld.compatible = true
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, nil, nil)
	native := &nativeDispatcher{}
	ld.innerDisp = native
	original := &session.Inbound{User: &protocol.MemoryUser{Email: email}, Source: xnet.TCPDestination(xnet.ParseAddress("127.0.0.1"), 1234), CanSpliceCopy: 2}
	ctx, cancel := context.WithCancel(session.ContextWithInbound(context.Background(), original))
	defer cancel()
	link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	if err := ld.DispatchLink(ctx, xnet.TCPDestination(xnet.ParseAddress("example.com"), 443), link); err != nil {
		t.Fatal(err)
	}
	if session.InboundFromContext(native.seen).CanSpliceCopy != 3 || original.CanSpliceCopy != 2 {
		t.Fatal("compatibility mode altered the caller's Vision state")
	}
	if _, ok := native.link.Reader.(*bandwidthReader); !ok {
		t.Fatal("compatible path is missing its limiter")
	}
	ld.CloseUser(email)
}

func newTestDispatcher() *LimitDispatcher {
	return &LimitDispatcher{
		limitedIPs: make(map[string]map[string]int),
	}
}

type nopReader struct{}

func (nopReader) ReadMultiBuffer() (buf.MultiBuffer, error) { return nil, nil }

func TestLimitDispatcher_DeviceLimitCheck(t *testing.T) {
	ld := newTestDispatcher()

	users := []model.UserSpec{
		{ID: 1, UUID: "uuid-1", DeviceLimit: 2, SpeedLimit: 0},
		{ID: 2, UUID: "uuid-2", DeviceLimit: 0, SpeedLimit: 10},
	}

	emailToUID := make(map[string]int)
	deviceLimits := make(map[string]int)
	speedLimits := make(map[string]int)
	for _, u := range users {
		email := userEmail(u.ID)
		emailToUID[email] = u.ID
		if u.DeviceLimit > 0 {
			deviceLimits[email] = u.DeviceLimit
		}
		if u.SpeedLimit > 0 {
			speedLimits[email] = u.SpeedLimit
		}
	}
	ld.UpdateLimits(emailToUID, deviceLimits, speedLimits)

	email1 := userEmail(1)

	// First IP should be allowed
	if ld.checkDeviceLimit(email1, "1.1.1.1", true) {
		t.Error("first IP should be allowed")
	}

	// Second IP should be allowed (limit=2)
	if ld.checkDeviceLimit(email1, "2.2.2.2", true) {
		t.Error("second IP should be allowed")
	}

	// Third unique IP should be rejected
	if !ld.checkDeviceLimit(email1, "3.3.3.3", true) {
		t.Error("third IP should be rejected (limit=2)")
	}

	// Same IP as first should be allowed (already connected)
	if ld.checkDeviceLimit(email1, "1.1.1.1", true) {
		t.Error("same IP should always be allowed")
	}

	// User 2 has no device limit — should always be allowed
	email2 := userEmail(2)
	for i := 0; i < 10; i++ {
		ip := "10.0.0." + string(rune('0'+i))
		if ld.checkDeviceLimit(email2, ip, true) {
			t.Errorf("user with no device limit should always be allowed (ip=%s)", ip)
		}
	}
}

func TestLimitDispatcher_DelConn(t *testing.T) {
	ld := newTestDispatcher()

	email := userEmail(1)
	deviceLimits := map[string]int{email: 2}
	ld.UpdateLimits(map[string]int{email: 1}, deviceLimits, nil)

	// Add 2 IPs
	ld.checkDeviceLimit(email, "1.1.1.1", true)
	ld.checkDeviceLimit(email, "2.2.2.2", true)

	// Third should be rejected
	if !ld.checkDeviceLimit(email, "3.3.3.3", true) {
		t.Error("third IP should be rejected")
	}

	// Remove first IP
	ld.delConn(email, "1.1.1.1")

	// Now third IP should be allowed
	if ld.checkDeviceLimit(email, "3.3.3.3", true) {
		t.Error("after deleting one IP, new IP should be allowed")
	}
}

func TestLimitDispatcher_GetConnectionState(t *testing.T) {
	ld := newTestDispatcher()

	email1 := userEmail(1)
	email2 := userEmail(2)
	ld.UpdateLimits(map[string]int{email1: 1, email2: 2}, nil, nil)

	ic1 := &ipCounter{}
	r1 := &atomic.Int64{}
	r1.Store(1)
	ic1.ips.Store("1.1.1.1", r1)
	r2 := &atomic.Int64{}
	r2.Store(1)
	ic1.ips.Store("2.2.2.2", r2)
	ld.unlimitedIPs.Store(email1, ic1)

	ic2 := &ipCounter{}
	r3 := &atomic.Int64{}
	r3.Store(1)
	ic2.ips.Store("3.3.3.3", r3)
	ld.unlimitedIPs.Store(email2, ic2)

	ld.connCount.Store(5)

	aliveIPs, connCount := ld.GetConnectionState()

	if connCount != 5 {
		t.Errorf("expected connCount=5, got %d", connCount)
	}
	if len(aliveIPs[1]) != 2 {
		t.Errorf("user 1 IPs: got %d, want 2", len(aliveIPs[1]))
	}
	if len(aliveIPs[2]) != 1 {
		t.Errorf("user 2 IPs: got %d, want 1", len(aliveIPs[2]))
	}
}

func TestLimitDispatcher_ResetConns(t *testing.T) {
	ld := newTestDispatcher()

	ld.mu.Lock()
	ld.limitedIPs["user@1"] = map[string]int{"1.1.1.1": 1}
	ld.mu.Unlock()
	ld.connCount.Store(3)

	ld.ResetConns()

	ld.mu.RLock()
	ipCount := len(ld.limitedIPs)
	ld.mu.RUnlock()
	if ipCount != 0 {
		t.Error("limitedIPs should be empty after reset")
	}

	if ld.connCount.Load() != 0 {
		t.Error("connCount should be 0 after reset")
	}
}

func TestLimitDispatcher_UnlimitedUserFastPath(t *testing.T) {
	ld := newTestDispatcher()

	email := userEmail(1)
	// No device limit set for this user
	ld.UpdateLimits(map[string]int{email: 1}, nil, nil)

	// Should use fast path (sync.Map), no lock needed
	for i := 0; i < 100; i++ {
		ip := "10.0.0." + string(rune('0'+i%10))
		if ld.checkDeviceLimit(email, ip, true) {
			t.Errorf("unlimited user should always be allowed (ip=%s)", ip)
		}
	}

	// Verify IPs are tracked in unlimitedIPs
	v, ok := ld.unlimitedIPs.Load(email)
	if !ok {
		t.Error("unlimited user should have entry in unlimitedIPs")
	}
	ic := v.(*ipCounter)
	ips := ic.aliveIPs()
	if len(ips) == 0 {
		t.Error("should have tracked some IPs")
	}
}

func TestLimitDispatcher_TrackLinkPreservesReader(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)

	origReader := nopReader{}
	origWriter := &closeTrackingWriter{Writer: buf.Discard, onClose: func() {}}
	link := &transport.Link{Reader: origReader, Writer: origWriter}

	ld.trackLink(link, email, "1.1.1.1", true)

	if link.Reader != origReader {
		t.Fatal("trackLink must not replace link.Reader")
	}
	if link.Writer == origWriter {
		t.Fatal("trackLink should wrap link.Writer for lifecycle callbacks")
	}
}

func TestLimitDispatcher_CloseTrackingWriterReleasesConn(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	ld.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	if ld.checkDeviceLimit(email, "1.1.1.1", true) {
		t.Fatal("first connection should be allowed")
	}

	link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	ld.trackLink(link, email, "1.1.1.1", true)

	if got := ld.connCount.Load(); got != 1 {
		t.Fatalf("expected connCount=1 after tracking, got %d", got)
	}
	cw, ok := link.Writer.(*closeTrackingWriter)
	if !ok {
		t.Fatal("expected closeTrackingWriter wrapper")
	}
	if err := cw.Close(); err != nil {
		t.Fatalf("closeTrackingWriter.Close() error = %v", err)
	}
	if got := ld.connCount.Load(); got != 0 {
		t.Fatalf("expected connCount=0 after close, got %d", got)
	}
	if ld.checkDeviceLimit(email, "2.2.2.2", true) {
		t.Fatal("device slot should be released after writer close")
	}
}
