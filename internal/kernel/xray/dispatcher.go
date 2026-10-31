// SPDX-License-Identifier: MPL-2.0

package xray

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	_ "unsafe"

	xrayDispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
	"golang.org/x/time/rate"

	"github.com/kirakirai8023/Xboard-Node/internal/nlog"
)

// Access xray's internal config creator registry so we can replace the
// default dispatcher factory with ours. This runs AFTER xray's init()
// functions because our package imports xray (dependency order guarantee).
//
//go:linkname typeCreatorRegistry github.com/xtls/xray-core/common.typeCreatorRegistry
var typeCreatorRegistry map[reflect.Type]common.ConfigCreator

var origDispatcherFactory common.ConfigCreator

// globalLimitDispatcher is set when the factory creates a LimitDispatcher.
// The Xray kernel reads it to configure limits and get connections.
var globalLimitDispatcher atomic.Pointer[LimitDispatcher]

func init() {
	configType := reflect.TypeOf((*xrayDispatcher.Config)(nil))
	origDispatcherFactory = typeCreatorRegistry[configType]
	typeCreatorRegistry[configType] = limitDispatcherFactory
}

func limitDispatcherFactory(ctx context.Context, config interface{}) (interface{}, error) {
	orig, err := origDispatcherFactory(ctx, config)
	if err != nil {
		return nil, err
	}
	inner, ok := orig.(routing.Dispatcher)
	if !ok {
		return orig, nil
	}
	ld := &LimitDispatcher{
		inner:      orig,
		innerDisp:  inner,
		limitedIPs: make(map[string]map[string]int),
	}
	globalLimitDispatcher.Store(ld)
	nlog.Core().Debug("xray: limit dispatcher installed")
	return ld, nil
}

// LimitDispatcher wraps xray's DefaultDispatcher to enforce per-user
// admission checks before a request is dispatched into xray-core.
//
// Links returned to inbounds retain concrete pipe readers for mux/XUDP.
// Rate limiting wraps writers, or a private copy of a DispatchLink link.
type LimitDispatcher struct {
	inner     interface{}        // original DefaultDispatcher (Feature + Dispatcher)
	innerDisp routing.Dispatcher // same object, typed as Dispatcher

	// limitedUsers: users with device limit > 0, protected by mu.
	// Admission and slot reservation use the same lock for TCP and UDP.
	mu           sync.RWMutex
	limitedIPs   map[string]map[string]int // email → sourceIP → refcount
	deviceLimits map[string]int            // email → max devices
	emailToUID   map[string]int            // email → panel user ID
	speedLimits  map[string]*rate.Limiter  // email/UUID → shared limiter
	bandwidth    map[string]*atomic.Pointer[rate.Limiter]
	connections  map[string]map[*managedLink]struct{}
	compatible   bool // immutable after the instance starts
	closed       bool

	// unlimitedIPs retains occupancy across device-limit policy changes.
	// Each entry is *ipCounter{ips sync.Map}.
	unlimitedIPs sync.Map // email → *ipCounter

	connCount atomic.Int64 // total active connections tracked by dispatcher
}

// ipCounter tracks IPs for unlimited users without any lock.
type ipCounter struct {
	ips sync.Map // sourceIP → *atomic.Int64 (refcount)
}

// aliveIPs returns a snapshot of distinct IPs.
func (ic *ipCounter) aliveIPs() map[string]bool {
	result := make(map[string]bool)
	ic.ips.Range(func(key, _ interface{}) bool {
		if rv, ok := ic.ips.Load(key); ok && rv.(*atomic.Int64).Load() > 0 {
			result[key.(string)] = true
		}
		return true
	})
	return result
}

// ─── routing.Dispatcher ──────────────────────────────────────────────────────

func (d *LimitDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	if !dest.IsValid() {
		return nil, errors.New("invalid destination")
	}
	ctx = d.canonicalUserContext(ctx)
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return nil, err
	}
	if email == "" {
		return d.innerDisp.Dispatch(ctx, dest)
	}
	ctx, cancel := context.WithCancel(ctx)
	if !d.compatible && d.speedLimiter(email) == nil {
		link, err := d.innerDisp.Dispatch(ctx, dest)
		if err != nil {
			cancel()
			d.delConn(email, sourceIP)
			return nil, err
		}
		if !d.trackManagedLink(ctx, cancel, link, email, sourceIP, isTCP, true) {
			return nil, errors.New("dispatcher closed")
		}
		return link, nil
	}
	ctx = disableSplice(ctx)

	// Own both pipe ends so both directions can be throttled at writes,
	// without changing the concrete reader returned to mux/XUDP callers.
	options := pipe.OptionsFromContext(ctx)
	upReader, upWriter := pipe.New(options...)
	downReader, downWriter := pipe.New(options...)
	link := &transport.Link{
		Reader: downReader,
		Writer: d.limitWriter(ctx, email, upWriter),
	}
	if !d.trackManagedLink(ctx, cancel, link, email, sourceIP, isTCP, false) {
		common.Interrupt(upReader)
		common.Interrupt(downWriter)
		return nil, errors.New("dispatcher closed")
	}
	outbound := &transport.Link{
		Reader: upReader,
		Writer: d.limitWriter(ctx, email, downWriter),
	}
	go func() {
		if err := d.innerDisp.DispatchLink(ctx, dest, outbound); err != nil {
			common.Interrupt(link.Writer)
			common.Interrupt(outbound.Writer)
			common.Interrupt(upReader)
			common.Interrupt(downReader)
		}
	}()
	return link, nil
}

func (d *LimitDispatcher) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	ctx = d.canonicalUserContext(ctx)
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return err
	}
	if email == "" {
		return d.innerDisp.DispatchLink(ctx, dest, link)
	}
	ctx, cancel := context.WithCancel(ctx)
	forwarded := *link
	native := !d.compatible && d.speedLimiter(email) == nil
	if !native {
		ctx = disableSplice(ctx)
	}
	if !d.trackManagedLink(ctx, cancel, &forwarded, email, sourceIP, isTCP, native) {
		return errors.New("dispatcher closed")
	}
	if native {
		err := d.innerDisp.DispatchLink(ctx, dest, &forwarded)
		if err != nil {
			common.Interrupt(forwarded.Writer)
			cancel()
		}
		return err
	}
	// Do not replace the caller's reader: mux owns its concrete pipe type.
	state := d.bandwidthState(email)
	limited := &transport.Link{
		Reader: &bandwidthReader{Reader: forwarded.Reader, ctx: ctx, limiter: state.Load},
		Writer: d.limitWriter(ctx, email, forwarded.Writer),
	}
	err = d.innerDisp.DispatchLink(ctx, dest, limited)
	if err != nil {
		common.Interrupt(limited.Writer)
	}
	return err
}

// canonicalUserContext maps upstream SOCKS/HTTP usernames to the same email
// used by traffic polling and limits, without mutating a shared session user.
func (d *LimitDispatcher) canonicalUserContext(ctx context.Context) context.Context {
	inbound := session.InboundFromContext(ctx)
	if inbound == nil || inbound.User == nil {
		return ctx
	}
	d.mu.RLock()
	uid := d.emailToUID[inbound.User.Email]
	d.mu.RUnlock()
	if uid == 0 {
		return ctx
	}
	if inbound.User.Email == userEmail(uid) {
		// Vision updates this session's readiness after its handshake. Keep
		// the original session shared with the protocol and outbound.
		return ctx
	}
	copyInbound, copyUser := *inbound, *inbound.User
	copyUser.Email = userEmail(uid)
	copyInbound.User = &copyUser
	return session.ContextWithInbound(ctx, &copyInbound)
}

func disableSplice(ctx context.Context) context.Context {
	if inbound := session.InboundFromContext(ctx); inbound != nil {
		copyInbound := *inbound
		copyInbound.CanSpliceCopy = 3
		return session.ContextWithInbound(ctx, &copyInbound)
	}
	return ctx
}

// identifyAndCheck extracts user identity from the session context, enforces
// device limits, and returns the user's email, source IP, and TCP flag.
// Returns a non-nil error only when the connection should be rejected.
func (d *LimitDispatcher) identifyAndCheck(ctx context.Context, dest net.Destination) (email, sourceIP string, isTCP bool, err error) {
	si := session.InboundFromContext(ctx)
	if si == nil || si.User == nil || len(si.User.Email) == 0 {
		return "", "", false, nil
	}
	email = si.User.Email
	sourceIP = si.Source.Address.IP().String()
	isTCP = dest.Network == net.Network_TCP

	if d.checkDeviceLimit(email, sourceIP, isTCP) {
		nlog.Core().Debug("xray: device limit exceeded", "email", email, "ip", sourceIP)
		return "", "", false, errors.New("device limit exceeded for " + email)
	}
	return email, sourceIP, isTCP, nil
}

// trackLink records connection lifecycle without mutating xray-core owned
// transport primitives. This keeps mux/XUDP compatible while still allowing
// the dispatcher to release device-limit state when the link closes.
func (d *LimitDispatcher) trackLink(link *transport.Link, email, sourceIP string, isTCP bool) {
	ctx, cancel := context.WithCancel(context.Background())
	d.trackManagedLink(ctx, cancel, link, email, sourceIP, isTCP, false)
}

type managedLink struct {
	native bool
	close  func()
}

func (d *LimitDispatcher) trackManagedLink(ctx context.Context, cancel context.CancelFunc, link *transport.Link, email, sourceIP string, isTCP, native bool) bool {
	record := &managedLink{native: native}
	var once sync.Once
	onClose := func() {
		once.Do(func() {
			cancel()
			d.delConn(email, sourceIP)
			d.connCount.Add(-1)
			d.mu.Lock()
			delete(d.connections[email], record)
			if len(d.connections[email]) == 0 {
				delete(d.connections, email)
			}
			d.mu.Unlock()
		})
	}
	reader, writer := link.Reader, link.Writer
	inbound := session.InboundFromContext(ctx)
	record.close = func() {
		cancel()
		// Splice may bypass the link, so also interrupt the physical socket.
		if inbound != nil && inbound.Conn != nil {
			_ = inbound.Conn.Close()
		}
		common.Interrupt(reader)
		common.Interrupt(writer)
		onClose()
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		cancel()
		if inbound != nil && inbound.Conn != nil {
			_ = inbound.Conn.Close()
		}
		common.Interrupt(reader)
		common.Interrupt(writer)
		d.delConn(email, sourceIP)
		return false
	}
	d.connCount.Add(1)
	if d.connections == nil {
		d.connections = make(map[string]map[*managedLink]struct{})
	}
	if d.connections[email] == nil {
		d.connections[email] = make(map[*managedLink]struct{})
	}
	d.connections[email][record] = struct{}{}
	// Cover a limit update racing with native dispatch before registration.
	limited := native && d.speedLimits[email] != nil
	d.mu.Unlock()

	link.Writer = &closeTrackingWriter{
		Writer:  link.Writer,
		onClose: onClose,
	}
	context.AfterFunc(ctx, onClose)
	if limited {
		record.close()
	}
	return true
}

func (d *LimitDispatcher) CloseUser(email string) {
	d.mu.RLock()
	var records []*managedLink
	for record := range d.connections[email] {
		records = append(records, record)
	}
	d.mu.RUnlock()
	for _, record := range records {
		record.close()
	}
}

// ─── features.Feature (delegated) ───────────────────────────────────────────

func (d *LimitDispatcher) Type() interface{} { return routing.DispatcherType() }

func (d *LimitDispatcher) Start() error {
	if s, ok := d.inner.(interface{ Start() error }); ok {
		return s.Start()
	}
	return nil
}

func (d *LimitDispatcher) Close() error {
	d.ResetConns()
	if c, ok := d.inner.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ─── Limit management (called by Xray kernel) ──────────────────────────────

func (d *LimitDispatcher) UpdateLimits(emailToUID map[string]int, deviceLimits, _ map[string]int) {
	d.mu.Lock()
	d.emailToUID = emailToUID
	d.deviceLimits = deviceLimits
	d.mu.Unlock()

}

func (d *LimitDispatcher) ResetConns() {
	d.mu.Lock()
	d.closed = true
	var records []*managedLink
	for _, connections := range d.connections {
		for record := range connections {
			records = append(records, record)
		}
	}
	d.mu.Unlock()
	for _, record := range records {
		record.close()
	}
	d.mu.Lock()
	d.limitedIPs = make(map[string]map[string]int)
	d.mu.Unlock()

	// Clear unlimited IPs
	d.unlimitedIPs.Range(func(key, _ interface{}) bool {
		d.unlimitedIPs.Delete(key)
		return true
	})

	d.connCount.Store(0)
}

// GetConnectionState returns dispatcher-tracked alive IPs and connection count.
// Traffic bytes are intentionally left to xray's built-in stats pipeline.
func (d *LimitDispatcher) GetConnectionState() (aliveIPs map[int]map[string]bool, connCount int) {
	d.mu.RLock()
	emailToUID := d.emailToUID
	limitedIPs := d.limitedIPs

	aliveIPs = make(map[int]map[string]bool)

	// Collect IPs from limited users (under RLock snapshot).
	for email, ipsMap := range limitedIPs {
		uid := emailToUID[email]
		if uid == 0 {
			continue
		}
		ipSet := make(map[string]bool, len(ipsMap))
		for ip := range ipsMap {
			ipSet[ip] = true
		}
		if len(ipSet) > 0 {
			aliveIPs[uid] = ipSet
		}
	}
	d.mu.RUnlock()

	// Collect IPs from unlimited users (lock-free).
	d.unlimitedIPs.Range(func(key, value interface{}) bool {
		email := key.(string)
		uid := emailToUID[email]
		if uid == 0 {
			return true
		}
		ic := value.(*ipCounter)
		if ips := ic.aliveIPs(); len(ips) > 0 {
			// Merge with limited IPs if any
			if existing, ok := aliveIPs[uid]; ok {
				for ip := range ips {
					existing[ip] = true
				}
			} else {
				aliveIPs[uid] = ips
			}
		}
		return true
	})

	connCount = int(d.connCount.Load())
	return
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// checkDeviceLimit atomically checks and reserves a source-IP slot. Existing
// sessions remain valid when a limit changes; new distinct IPs cannot exceed it.
func (d *LimitDispatcher) checkDeviceLimit(email, sourceIP string, _ bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return true
	}
	limit := d.deviceLimits[email]
	ips := d.limitedIPs[email]
	var unlimited *ipCounter
	if value, ok := d.unlimitedIPs.Load(email); ok {
		unlimited = value.(*ipCounter)
	}
	if unlimited != nil {
		if value, ok := unlimited.ips.Load(sourceIP); ok {
			value.(*atomic.Int64).Add(1)
			return false
		}
	}
	if ips[sourceIP] > 0 {
		ips[sourceIP]++
		return false
	}
	if limit > 0 {
		count := len(ips)
		if unlimited != nil {
			unlimited.ips.Range(func(ip, _ any) bool {
				if ips[ip.(string)] == 0 {
					count++
				}
				return true
			})
		}
		if count >= limit {
			return true
		}
	}
	if limit <= 0 {
		value, _ := d.unlimitedIPs.LoadOrStore(email, &ipCounter{})
		counter := &atomic.Int64{}
		counter.Store(1)
		value.(*ipCounter).ips.Store(sourceIP, counter)
	} else {
		if ips == nil {
			ips = make(map[string]int)
			d.limitedIPs[email] = ips
		}
		ips[sourceIP]++
	}
	return false
}

// delConn releases either a TCP or UDP slot, including sessions created before
// the user's device-limit policy changed.
func (d *LimitDispatcher) delConn(email, sourceIP string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if value, ok := d.unlimitedIPs.Load(email); ok {
		ic := value.(*ipCounter)
		if value, ok := ic.ips.Load(sourceIP); ok {
			if value.(*atomic.Int64).Add(-1) <= 0 {
				ic.ips.Delete(sourceIP)
			}
			return
		}
	}
	if ips := d.limitedIPs[email]; ips != nil {
		ips[sourceIP]--
		if ips[sourceIP] <= 0 {
			delete(ips, sourceIP)
		}
		if len(ips) == 0 {
			delete(d.limitedIPs, email)
		}
	}
}

type closeTrackingWriter struct {
	buf.Writer
	onClose func()
	closed  atomic.Bool
}

func (w *closeTrackingWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	return common.Close(w.Writer)
}

func (w *closeTrackingWriter) Interrupt() {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	common.Interrupt(w.Writer)
}
