// SPDX-License-Identifier: AGPL-3.0-only

package xray

import (
	"context"
	"fmt"

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
	return &bandwidthWriter{
		Writer: writer, ctx: ctx, cancel: cancel,
		limiter: func() *rate.Limiter { return d.speedLimiter(email) },
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
