// SPDX-License-Identifier: MPL-2.0

package singbox

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	tun "github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/time/rate"

	"github.com/kirakirai8023/Xboard-Node/internal/nlog"
)

// ipPool caches ipSnapshot maps to reduce allocations.
var ipPool = sync.Pool{
	New: func() interface{} {
		return make(map[string]struct{}, 16)
	},
}

// ─── Per-user statistics ────────────────────────────────────────────────────

// userStats holds per-user traffic counters and alive IP tracking.
// Traffic counters are lock-free atomics (read on every packet).
// IP tracking uses a lightweight mutex (only touched at connect/disconnect).
type userStats struct {
	upload    atomic.Int64
	download  atomic.Int64
	bandwidth atomic.Pointer[rate.Limiter]

	mu        sync.RWMutex   // RWMutex for concurrent reads
	ips       map[string]int // sourceIP → refcount (number of active conns from that IP)
	connCount int            // total active connections
}

// addConn registers a new connection from sourceIP.
func (u *userStats) addConn(sourceIP string) {
	u.mu.Lock()
	u.connCount++
	u.ips[sourceIP]++
	u.mu.Unlock()
}

// removeConn unregisters a connection from sourceIP.
func (u *userStats) removeConn(sourceIP string) {
	u.mu.Lock()
	u.connCount--
	u.ips[sourceIP]--
	if u.ips[sourceIP] <= 0 {
		delete(u.ips, sourceIP)
	}
	u.mu.Unlock()
}

// distinctIPs returns the number of distinct IPs currently connected.
func (u *userStats) distinctIPs() int {
	u.mu.Lock()
	n := len(u.ips)
	u.mu.Unlock()
	return n
}

// ipSnapshot returns a pooled copy of the current IP set.
func (u *userStats) ipSnapshot() map[string]struct{} {
	cp := ipPool.Get().(map[string]struct{})
	for k := range cp {
		delete(cp, k)
	}
	u.mu.Lock()
	for ip := range u.ips {
		cp[ip] = struct{}{}
	}
	u.mu.Unlock()
	return cp
}

// releaseIPSnapshot returns the snapshot to pool.
// Large maps are discarded to prevent memory bloat.
func releaseIPSnapshot(m map[string]struct{}) {
	// Discard large maps to prevent pool memory bloat
	if len(m) > 64 {
		return
	}
	ipPool.Put(m)
}

// aliveIPList returns the set of alive IPs as a boolean map (for reporting).
func (u *userStats) aliveIPList() map[string]bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.ips) == 0 {
		return nil
	}
	result := make(map[string]bool, len(u.ips))
	for ip := range u.ips {
		result[ip] = true
	}
	return result
}

// ─── ConnTracker ────────────────────────────────────────────────────────────

// ConnTracker provides per-user traffic counting, alive IP tracking, device
// limit gate-keeping, and per-user speed limiting for the sing-box kernel.
//
// Architecture: instead of maintaining a per-connection map (O(connections)),
// it maintains per-user atomic counters and IP refcount sets (O(users)).
// This means:
//   - GetUserTraffic is O(users), not O(connections)
//   - No Snapshot/pending buffer needed
//   - No per-connection map lock contention
//   - Bytes are counted directly into per-user atomics via CountFunc callbacks
//
// The per-connection wrapper (trackedConn) is still needed for:
//   - Hooking into sing's ReadCounter/WriteCounter for zero-copy byte counting
//   - Close callback to decrement IP refcounts
//   - Per-connection rate limit token accumulation (amortized WaitN)
type ConnTracker struct {
	usersMu   sync.RWMutex
	users     map[int]*userStats   // userID → stats
	uuidMap   map[string]int       // UUID → userID (for lookup in RoutedConnection)
	connMap   map[string]io.Closer // TCP and UDP connection lifecycle handles
	connUUIDs map[string]string

	idCounter atomic.Int64

	// speedLimitFunc resolves a user UUID to a *rate.Limiter.
	speedLimitFunc atomic.Pointer[func(uuid string) *rate.Limiter]

	// deviceLimitFunc resolves a user UUID to their device limit.
	deviceLimitFunc atomic.Pointer[func(uuid string) (int, bool)]

	// Multi-node device state from panel
	globalDevices    map[int]map[string]bool // userID → IP → exists
	globalMu         sync.RWMutex
	globalLastUpdate time.Time
}

// NewConnTracker creates a tracker.
func NewConnTracker(_ int) *ConnTracker {
	return &ConnTracker{
		users:         make(map[int]*userStats),
		uuidMap:       make(map[string]int),
		connMap:       make(map[string]io.Closer),
		connUUIDs:     make(map[string]string),
		globalDevices: make(map[int]map[string]bool),
	}
}

// SetSpeedLimitFunc configures the per-user speed limit lookup.
func (t *ConnTracker) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) {
	if fn == nil {
		t.speedLimitFunc.Store(nil)
		t.refreshSpeedLimits()
		return
	}
	t.speedLimitFunc.Store(&fn)
	t.refreshSpeedLimits()
}

// SetDeviceLimitFunc configures the per-user device limit lookup for gate-keeping.
func (t *ConnTracker) SetDeviceLimitFunc(fn func(uuid string) (int, bool)) {
	if fn == nil {
		t.deviceLimitFunc.Store(nil)
		return
	}
	t.deviceLimitFunc.Store(&fn)
}

// SetUserMap replaces the UUID→userID mapping and ensures per-user stats
// structs exist for all users. Old users that are no longer present keep
// their stats until their connections drain.
func (t *ConnTracker) SetUserMap(m map[string]int) {
	t.usersMu.Lock()
	t.uuidMap = m
	for _, uid := range m {
		if _, ok := t.users[uid]; !ok {
			t.users[uid] = &userStats{ips: make(map[string]int)}
		}
	}
	t.usersMu.Unlock()
	t.refreshSpeedLimits()
}

func (t *ConnTracker) refreshSpeedLimits() {
	t.usersMu.RLock()
	states := make(map[string]*userStats, len(t.uuidMap))
	for uuid, uid := range t.uuidMap {
		states[uuid] = t.users[uid]
	}
	t.usersMu.RUnlock()
	fn := t.speedLimitFunc.Load()
	for uuid, state := range states {
		var limit *rate.Limiter
		if fn != nil {
			limit = (*fn)(uuid)
		}
		previous := state.bandwidth.Swap(limit)
		if previous == nil && limit != nil {
			// An existing zero-copy transfer may bypass connection methods.
			// Reconnect only this user when a new limit becomes applicable.
			t.CloseByUUID(uuid)
		}
	}
}

// UpdateGlobalDevices syncs global device state from panel.
func (t *ConnTracker) UpdateGlobalDevices(users map[int][]string) {
	t.globalMu.Lock()
	t.globalDevices = make(map[int]map[string]bool, len(users))
	for uid, ips := range users {
		m := make(map[string]bool, len(ips))
		for _, ip := range ips {
			m[ip] = true
		}
		t.globalDevices[uid] = m
	}
	t.globalLastUpdate = time.Now()
	t.globalMu.Unlock()
	nlog.Core().Debug("global device state updated", "users", len(users))
}

// ClearGlobalDevices resets global device state (on WS disconnect).
func (t *ConnTracker) ClearGlobalDevices() {
	t.globalMu.Lock()
	t.globalDevices = make(map[int]map[string]bool)
	t.globalLastUpdate = time.Time{}
	t.globalMu.Unlock()
	nlog.Core().Debug("global device state cleared")
}

// ─── adapter.ConnectionTracker ──────────────────────────────────────────────

// RoutedFlow opts out of TUN flow offload tracking. Node proxy inbounds use
// RoutedConnection/RoutedPacketConnection for byte and device accounting.
func (t *ConnTracker) RoutedFlow(context.Context, adapter.InboundContext, adapter.Rule, adapter.Outbound) tun.FlowTracker {
	return nil
}

// RoutedConnection wraps a TCP conn to count bytes per-user, track IPs,
// and optionally rate-limit. Gate-keeps device limits at connection time.
func (t *ConnTracker) RoutedConnection(
	ctx context.Context, conn net.Conn,
	metadata adapter.InboundContext,
	_ adapter.Rule, _ adapter.Outbound,
) net.Conn {
	uuid := metadata.User
	sourceIP := metadata.Source.Addr.String()

	t.usersMu.RLock()
	uid := t.uuidMap[uuid]
	us := t.users[uid]
	t.usersMu.RUnlock()

	if !t.admitConnection(uuid, us, uid, sourceIP) {
		_ = conn.Close()
		return conn
	}

	connID := t.nextID()

	var lim *rate.Limiter
	if slf := t.speedLimitFunc.Load(); slf != nil {
		lim = (*slf)(uuid)
	}

	tracked := &trackedConn{
		Conn:     conn,
		tracker:  t,
		us:       us,
		userID:   uid,
		connID:   connID,
		sourceIP: sourceIP,
		limiter:  lim,
		ctx:      ctx,
	}
	t.usersMu.Lock()
	t.connMap[connID], t.connUUIDs[connID] = tracked, uuid
	t.usersMu.Unlock()
	return tracked
}

// RoutedPacketConnection wraps UDP with per-user counting and registers a
// lifecycle handle so targeted user disconnection also closes packet sessions.
func (t *ConnTracker) RoutedPacketConnection(
	ctx context.Context, conn N.PacketConn,
	metadata adapter.InboundContext,
	_ adapter.Rule, _ adapter.Outbound,
) N.PacketConn {
	uuid := metadata.User
	sourceIP := metadata.Source.Addr.String()

	t.usersMu.RLock()
	uid := t.uuidMap[uuid]
	us := t.users[uid]
	t.usersMu.RUnlock()

	if !t.admitConnection(uuid, us, uid, sourceIP) {
		_ = conn.Close()
		return conn
	}

	connID := t.nextID()

	var lim *rate.Limiter
	if slf := t.speedLimitFunc.Load(); slf != nil {
		lim = (*slf)(uuid)
	}

	tracked := &trackedPacketConn{
		PacketConn: conn,
		tracker:    t,
		us:         us,
		userID:     uid,
		connID:     connID,
		sourceIP:   sourceIP,
		limiter:    lim,
		ctx:        ctx,
	}
	t.usersMu.Lock()
	t.connMap[connID], t.connUUIDs[connID] = tracked, uuid
	t.usersMu.Unlock()
	return tracked
}

// admitConnection checks and reserves a device slot under the same user lock.
// TCP and UDP share the same source-IP refcounts.
func (t *ConnTracker) admitConnection(uuid string, us *userStats, uid int, sourceIP string) bool {
	if us == nil {
		return true
	}
	limit := 0
	if fn := t.deviceLimitFunc.Load(); fn != nil {
		if value, enabled := (*fn)(uuid); enabled {
			limit = value
		}
	}
	us.mu.Lock()
	defer us.mu.Unlock()
	if t.deviceLimitExceededLocked(us, uid, sourceIP, limit) {
		return false
	}
	us.connCount++
	us.ips[sourceIP]++
	return true
}

// checkDeviceGate probes admission without reserving a slot.
func (t *ConnTracker) checkDeviceGate(us *userStats, userID int, sourceIP string, limit int) bool {
	if us == nil {
		return false
	}
	us.mu.RLock()
	defer us.mu.RUnlock()
	return t.deviceLimitExceededLocked(us, userID, sourceIP, limit)
}

func (t *ConnTracker) deviceLimitExceededLocked(us *userStats, userID int, sourceIP string, limit int) bool {
	if limit <= 0 || us.ips[sourceIP] > 0 {
		return false
	}
	t.globalMu.RLock()
	defer t.globalMu.RUnlock()
	global := t.globalDevices[userID]
	if time.Since(t.globalLastUpdate) > 60*time.Second {
		global = nil
	}
	if global[sourceIP] {
		return false
	}
	count := len(us.ips)
	for ip := range global {
		if us.ips[ip] == 0 {
			count++
		}
	}
	return count >= limit
}

func (t *ConnTracker) nextID() string {
	return "sb-" + formatInt36(t.idCounter.Add(1))
}

func formatInt36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var buf [13]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%36]
		n /= 36
	}
	return string(buf[i:])
}

// ─── Public API ─────────────────────────────────────────────────────────────

// GetUserTraffic returns per-user cumulative traffic and alive IPs.
// This is O(users), not O(connections).
func (t *ConnTracker) GetUserTraffic() (traffic map[int][2]int64, aliveIPs map[int]map[string]bool, connCount int) {
	t.usersMu.RLock()
	traffic = make(map[int][2]int64, len(t.users))
	aliveIPs = make(map[int]map[string]bool, len(t.users))
	for uid, us := range t.users {
		up := us.upload.Load()
		down := us.download.Load()
		if up > 0 || down > 0 {
			traffic[uid] = [2]int64{up, down}
		}

		ips := us.aliveIPList()
		if ips != nil {
			aliveIPs[uid] = ips
		}

		us.mu.Lock()
		connCount += us.connCount
		us.mu.Unlock()
	}
	t.usersMu.RUnlock()
	return
}

// CloseByID force-closes a connection by its ID.
func (t *ConnTracker) CloseByID(id string) bool {
	t.usersMu.RLock()
	conn, ok := t.connMap[id]
	t.usersMu.RUnlock()
	if !ok {
		return false
	}
	if conn != nil {
		conn.Close()
	}
	return true
}

// CloseByUUID force-closes ALL connections for a given user UUID.
func (t *ConnTracker) CloseByUUID(uuid string) int {
	t.usersMu.RLock()
	var connections []io.Closer
	for id, candidate := range t.connUUIDs {
		if candidate == uuid {
			connections = append(connections, t.connMap[id])
		}
	}
	t.usersMu.RUnlock()
	for _, conn := range connections {
		_ = conn.Close()
	}
	return len(connections)
}

// ActiveCount returns the total number of active connections.
func (t *ConnTracker) ActiveCount() int {
	t.usersMu.RLock()
	total := 0
	for _, us := range t.users {
		us.mu.Lock()
		total += us.connCount
		us.mu.Unlock()
	}
	t.usersMu.RUnlock()
	return total
}

// removeConnRef removes the connection reference from connMap on close.
func (t *ConnTracker) removeConnRef(connID string) {
	t.usersMu.Lock()
	delete(t.connMap, connID)
	delete(t.connUUIDs, connID)
	t.usersMu.Unlock()
}

// ─── trackedConn (TCP) ──────────────────────────────────────────────────────

// waitTokens accepts copy batches larger than the bucket capacity and remains
// cancellable. Count callbacks from Linux splice can contain up to 1 MiB.
func waitTokens(ctx context.Context, limiter *rate.Limiter, bytes int64) error {
	if limiter == nil || limiter.Limit() == rate.Inf {
		return nil
	}
	for bytes > 0 {
		capacity := limiter.Burst()
		if capacity < 1 {
			return fmt.Errorf("rate limiter has no burst capacity")
		}
		count := int64(capacity)
		if bytes < count {
			count = bytes
		}
		if err := limiter.WaitN(ctx, int(count)); err != nil {
			return err
		}
		bytes -= count
	}
	return nil
}

func writeThrottled(ctx context.Context, limiter *rate.Limiter, writer io.Writer, data []byte) (written int, err error) {
	for len(data) > 0 {
		chunk := data
		if limiter != nil && limiter.Limit() != rate.Inf {
			capacity := limiter.Burst()
			if capacity < 1 {
				return written, fmt.Errorf("rate limiter has no burst capacity")
			}
			if len(chunk) > capacity {
				chunk = chunk[:capacity]
			}
		}
		if err = waitTokens(ctx, limiter, int64(len(chunk))); err != nil {
			return
		}
		var n int
		n, err = writer.Write(chunk)
		written += n
		if err != nil {
			return
		}
		if n != len(chunk) {
			return written, io.ErrShortWrite
		}
		data = data[n:]
	}
	return written, nil
}

type RateLimitedWriter struct {
	writer  io.Writer
	limiter *rate.Limiter
	ctx     context.Context
}

func (w *RateLimitedWriter) Write(b []byte) (int, error) {
	return writeThrottled(w.ctx, w.limiter, w.writer, b)
}

type RateLimitedReadCloser struct {
	reader  io.Reader
	limiter *rate.Limiter
	ctx     context.Context
}

func (r *RateLimitedReadCloser) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	if n > 0 {
		if waitErr := waitTokens(r.ctx, r.limiter, int64(n)); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

type trackedConn struct {
	net.Conn
	tracker  *ConnTracker
	us       *userStats // per-user stats (upload/download atomics + IP tracking)
	userID   int        // user ID for device tracking
	connID   string
	sourceIP string
	limiter  *rate.Limiter
	ctx      context.Context
	closed   atomic.Bool
}

func (c *trackedConn) currentLimiter() *rate.Limiter {
	if c.us != nil {
		return c.us.bandwidth.Load()
	}
	return c.limiter
}

func (c *trackedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		if c.us != nil {
			c.us.upload.Add(int64(n))
		}
		if waitErr := waitTokens(c.ctx, c.currentLimiter(), int64(n)); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}
func (c *trackedConn) Write(b []byte) (int, error) {
	n, err := writeThrottled(c.ctx, c.currentLimiter(), c.Conn, b)
	if n > 0 && c.us != nil {
		c.us.download.Add(int64(n))
	}
	return n, err
}

func (c *trackedConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		if c.us != nil {
			c.us.removeConn(c.sourceIP)
		}
		c.tracker.removeConnRef(c.connID)
	}
	return c.Conn.Close()
}

// makeCountFunc builds a CountFunc for zero-copy byte counting via sing's
// ReadCounter/WriteCounter unwrap interfaces.
func (c *trackedConn) makeCountFunc(counter *atomic.Int64) N.CountFunc {
	return func(n int64) {
		counter.Add(n)
		if err := waitTokens(c.ctx, c.currentLimiter(), n); err != nil {
			_ = c.Close()
		}
	}
}

func (c *trackedConn) UnwrapReader() (io.Reader, []N.CountFunc) {
	if c.us == nil {
		return c.Conn, nil
	}
	return c.Conn, []N.CountFunc{c.makeCountFunc(&c.us.upload)} // 从入站读取 = 用户上传
}

func (c *trackedConn) UnwrapWriter() (io.Writer, []N.CountFunc) {
	if c.us == nil {
		return c.Conn, nil
	}
	return c.Conn, []N.CountFunc{c.makeCountFunc(&c.us.download)} // 向入站写入 = 用户下载
}

func (c *trackedConn) Upstream() any           { return c.Conn }
func (c *trackedConn) ReaderReplaceable() bool { return true }
func (c *trackedConn) WriterReplaceable() bool { return true }

// ─── trackedPacketConn (UDP / QUIC) ─────────────────────────────────────────

type trackedPacketConn struct {
	N.PacketConn
	tracker  *ConnTracker
	us       *userStats
	userID   int
	connID   string
	sourceIP string
	limiter  *rate.Limiter
	ctx      context.Context
	closed   atomic.Bool
}

func (c *trackedPacketConn) currentLimiter() *rate.Limiter {
	if c.us != nil {
		return c.us.bandwidth.Load()
	}
	return c.limiter
}

func (c *trackedPacketConn) ReadPacket(buffer *buf.Buffer) (singM.Socksaddr, error) {
	dest, err := c.PacketConn.ReadPacket(buffer)
	if err == nil {
		n := int64(buffer.Len())
		if c.us != nil {
			c.us.upload.Add(n)
		}
		if waitErr := waitTokens(c.ctx, c.currentLimiter(), n); waitErr != nil {
			return dest, waitErr
		}
	}
	return dest, err
}
func (c *trackedPacketConn) WritePacket(buffer *buf.Buffer, dest singM.Socksaddr) error {
	n := int64(buffer.Len())
	if err := waitTokens(c.ctx, c.currentLimiter(), n); err != nil {
		return err
	}
	err := c.PacketConn.WritePacket(buffer, dest)
	if err == nil && c.us != nil {
		c.us.download.Add(n)
	}
	return err
}

func (c *trackedPacketConn) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		if c.us != nil {
			c.us.removeConn(c.sourceIP)
		}
		c.tracker.removeConnRef(c.connID)
	}
	return c.PacketConn.Close()
}

func (c *trackedPacketConn) makeCountFunc(counter *atomic.Int64) N.CountFunc {
	return func(n int64) {
		counter.Add(n)
		if err := waitTokens(c.ctx, c.currentLimiter(), n); err != nil {
			_ = c.Close()
		}
	}
}

func (c *trackedPacketConn) UnwrapPacketReader() (N.PacketReader, []N.CountFunc) {
	if c.us == nil {
		return c.PacketConn, nil
	}
	return c.PacketConn, []N.CountFunc{c.makeCountFunc(&c.us.download)}
}

func (c *trackedPacketConn) UnwrapPacketWriter() (N.PacketWriter, []N.CountFunc) {
	if c.us == nil {
		return c.PacketConn, nil
	}
	return c.PacketConn, []N.CountFunc{c.makeCountFunc(&c.us.upload)}
}

func (c *trackedPacketConn) Upstream() any           { return c.PacketConn }
func (c *trackedPacketConn) ReaderReplaceable() bool { return true }
func (c *trackedPacketConn) WriterReplaceable() bool { return true }
