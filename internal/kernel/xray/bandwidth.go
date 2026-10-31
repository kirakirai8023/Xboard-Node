// SPDX-License-Identifier: AGPL-3.0-only

package xray

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"golang.org/x/time/rate"
)

func (d *LimitDispatcher) speedLimiter(email string) *rate.Limiter {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.speedLimits[email]
}

func (d *LimitDispatcher) limitWriter(ctx context.Context, email string, writer buf.Writer) buf.Writer {
	ctx, cancel := context.WithCancel(ctx)
	state := d.bandwidthState(email)
	return &bandwidthWriter{
		Writer: writer, ctx: ctx, cancel: cancel,
		limiter: state.Load,
	}
}

// Each limited connection retains its user's atomic cell. Refreshing limits
// does not require a global map lookup or mutex for every buffer.
func (d *LimitDispatcher) bandwidthState(email string) *atomic.Pointer[rate.Limiter] {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bandwidth == nil {
		d.bandwidth = make(map[string]*atomic.Pointer[rate.Limiter])
	}
	state := d.bandwidth[email]
	if state == nil {
		state = new(atomic.Pointer[rate.Limiter])
		state.Store(d.speedLimits[email])
		d.bandwidth[email] = state
	}
	return state
}

func (d *LimitDispatcher) UpdateSpeedLimits(limits map[string]*rate.Limiter) {
	d.mu.Lock()
	d.speedLimits = limits
	for email, state := range d.bandwidth {
		state.Store(limits[email])
		if _, exists := limits[email]; !exists {
			delete(d.bandwidth, email)
		}
	}
	var reconnect []*managedLink
	for email, connections := range d.connections {
		if limits[email] != nil {
			for record := range connections {
				if record.native {
					reconnect = append(reconnect, record)
				}
			}
		}
	}
	d.mu.Unlock()
	for _, record := range reconnect {
		record.close()
	}
}

// waitBandwidth splits large buffers into burst-sized reservations. Both
// directions and all connections for a user consume the same token bucket.
func waitBandwidth(ctx context.Context, lim *rate.Limiter, size int32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if lim == nil || lim.Limit() == rate.Inf {
		return nil
	}
	for size > 0 {
		burst := lim.Burst()
		if burst <= 0 {
			return fmt.Errorf("bandwidth limiter has no burst capacity")
		}
		n := min(int(size), burst)
		if err := lim.WaitN(ctx, n); err != nil {
			return err
		}
		size -= int32(n)
	}
	return nil
}

type bandwidthWriter struct {
	buf.Writer
	ctx     context.Context
	cancel  context.CancelFunc
	limiter func() *rate.Limiter
}

func (w *bandwidthWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	if err := waitBandwidth(w.ctx, w.limiter(), mb.Len()); err != nil {
		buf.ReleaseMulti(mb)
		return err
	}
	return w.Writer.WriteMultiBuffer(mb)
}

func (w *bandwidthWriter) Close() error {
	w.cancel()
	return common.Close(w.Writer)
}

func (w *bandwidthWriter) Interrupt() {
	w.cancel()
	common.Interrupt(w.Writer)
}

type bandwidthReader struct {
	buf.Reader
	ctx     context.Context
	limiter func() *rate.Limiter
}

func (r *bandwidthReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	if waitErr := waitBandwidth(r.ctx, r.limiter(), mb.Len()); waitErr != nil {
		buf.ReleaseMulti(mb)
		return nil, waitErr
	}
	return mb, err
}

func (r *bandwidthReader) Interrupt() {
	common.Interrupt(r.Reader)
}

func (r *bandwidthReader) Close() error {
	return common.Close(r.Reader)
}
