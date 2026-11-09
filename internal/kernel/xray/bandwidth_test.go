// SPDX-License-Identifier: AGPL-3.0-only

package xray

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/mux"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
	"golang.org/x/time/rate"
)

func TestBandwidthWriterHonorsCancellationAndUpdates(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(1)
	lim := rate.NewLimiter(1, 1)
	ld.speedLimits = map[string]*rate.Limiter{email: lim}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sink bytes.Buffer
	writer := ld.limitWriter(ctx, email, buf.NewWriter(&sink))
	first := buf.New()
	_, _ = first.Write([]byte("a"))
	if err := writer.WriteMultiBuffer(buf.MultiBuffer{first}); err != nil {
		t.Fatal(err)
	}
	// Existing wrappers must pick up a refreshed limiter without reconnecting.
	ld.UpdateSpeedLimits(map[string]*rate.Limiter{})
	third := buf.New()
	_, _ = third.Write([]byte("c"))
	if err := writer.WriteMultiBuffer(buf.MultiBuffer{third}); err != nil {
		t.Fatal(err)
	}
	if sink.String() != "ac" {
		t.Fatalf("updated write = %q", sink.String())
	}
	cancel()
	second := buf.New()
	_, _ = second.Write([]byte("b"))
	if err := writer.WriteMultiBuffer(buf.MultiBuffer{second}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	if sink.String() != "ac" {
		t.Fatalf("canceled write forwarded data: %q", sink.String())
	}
}

func TestWaitBandwidthHandlesBuffersLargerThanBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := waitBandwidth(ctx, rate.NewLimiter(1<<20, 2), 17); err != nil {
		t.Fatalf("burst-sized reservations: %v", err)
	}
}

type memoryEchoDispatcher struct {
	routing.Dispatcher
	seen chan context.Context
}

func (d memoryEchoDispatcher) DispatchLink(ctx context.Context, _ xnet.Destination, link *transport.Link) error {
	defer common.Close(link.Writer)
	mb, err := link.Reader.ReadMultiBuffer()
	if err != nil {
		return err
	}
	err = link.Writer.WriteMultiBuffer(mb)
	d.seen <- ctx
	return err
}

func TestLimitDispatcherMemoryPipes(t *testing.T) {
	for _, useLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "Dispatch", true: "DispatchLink"}[useLink], func(t *testing.T) {
			ld := newTestDispatcher()
			email := userEmail(1)
			ld.UpdateLimits(map[string]int{email: 1, "uuid-1": 1}, nil, nil)
			ld.speedLimits = map[string]*rate.Limiter{email: rate.NewLimiter(1<<20, 1<<20)}
			seen := make(chan context.Context, 1)
			ld.innerDisp = memoryEchoDispatcher{seen: seen}
			inbound := &session.Inbound{
				User:          &protocol.MemoryUser{Email: "uuid-1"},
				Source:        xnet.TCPDestination(xnet.ParseAddress("127.0.0.1"), 12345),
				CanSpliceCopy: 1,
			}
			ctx, cancel := context.WithTimeout(session.ContextWithInbound(context.Background(), inbound), time.Second)
			defer cancel()
			dest := xnet.TCPDestination(xnet.ParseAddress("example.com"), 443)
			payload := []byte("shared limiter, original pipe")
			if useLink {
				reader := buf.NewReader(bytes.NewReader(payload))
				var sink bytes.Buffer
				link := &transport.Link{Reader: reader, Writer: buf.NewWriter(&sink)}
				if err := ld.DispatchLink(ctx, dest, link); err != nil {
					t.Fatal(err)
				}
				if link.Reader != reader || !bytes.Equal(sink.Bytes(), payload) {
					t.Fatal("DispatchLink changed its caller's reader or payload")
				}
			} else {
				link, err := ld.Dispatch(ctx, dest)
				if err != nil {
					t.Fatal(err)
				}
				defer common.Close(link.Writer)
				defer common.Interrupt(link.Reader)
				reader, ok := link.Reader.(*pipe.Reader)
				if !ok {
					t.Fatalf("mux/XUDP reader type = %T", link.Reader)
				}
				mb := buf.New()
				_, _ = mb.Write(payload)
				if err := link.Writer.WriteMultiBuffer(buf.MultiBuffer{mb}); err != nil {
					t.Fatal(err)
				}
				response, err := reader.ReadMultiBufferTimeout(time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer buf.ReleaseMulti(response)
				var sink bytes.Buffer
				for _, b := range response {
					_, _ = sink.Write(b.Bytes())
				}
				if !bytes.Equal(sink.Bytes(), payload) {
					t.Fatal("Dispatch corrupted the bidirectional pipe payload")
				}
			}
			select {
			case forwarded := <-seen:
				got := session.InboundFromContext(forwarded)
				if got.User.Email != email || got.CanSpliceCopy != 3 {
					t.Fatal("managed traffic bypasses canonical stats or limiter wrappers")
				}
				if inbound.User.Email != "uuid-1" || inbound.CanSpliceCopy != 1 {
					t.Fatal("dispatcher mutated its caller's session")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

// Exercise upstream mux framing and concurrent writers through our limiter
// using only memory pipes, without opening proxy listeners on the test host.
func TestLimitDispatcher_MuxConcurrentStreams(t *testing.T) {
	for _, network := range []xnet.Network{xnet.Network_TCP, xnet.Network_UDP} {
		t.Run(network.String(), func(t *testing.T) {
			const streams = 8
			email := userEmail(1)
			ld := newTestDispatcher()
			ld.UpdateLimits(map[string]int{email: 1}, nil, nil)
			ld.UpdateSpeedLimits(map[string]*rate.Limiter{email: rate.NewLimiter(1<<20, 1<<20)})
			ld.innerDisp = memoryEchoDispatcher{seen: make(chan context.Context, streams)}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			serverCtx := session.ContextWithInbound(ctx, &session.Inbound{
				User:   &protocol.MemoryUser{Email: email},
				Source: xnet.TCPDestination(xnet.LocalHostIP, 12345),
			})
			upR, upW := pipe.New(pipe.WithoutSizeLimit())
			downR, downW := pipe.New(pipe.WithoutSizeLimit())
			t.Cleanup(func() {
				ld.Close()
				common.Interrupt(upR)
				common.Interrupt(upW)
				common.Interrupt(downR)
				common.Interrupt(downW)
			})
			server, err := mux.NewServerWorker(serverCtx, ld, &transport.Link{Reader: upR, Writer: downW})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { server.Close() })
			client, err := mux.NewClientWorker(transport.Link{Reader: downR, Writer: upW}, mux.ClientStrategy{MaxConcurrency: streams})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { client.Close() })
			t.Run("streams", func(t *testing.T) {
				for i := range streams {
					t.Run(strconv.Itoa(i), func(t *testing.T) {
						t.Parallel()
						inR, inW := pipe.New(pipe.WithoutSizeLimit())
						outR, outW := pipe.New(pipe.WithoutSizeLimit())
						defer common.Interrupt(inR)
						defer common.Interrupt(inW)
						defer common.Interrupt(outR)
						defer common.Interrupt(outW)
						clientCtx := session.ContextWithOutbounds(ctx, []*session.Outbound{{
							Target: xnet.Destination{Network: network, Address: xnet.DomainAddress("example.com"), Port: 443},
						}})
						if !client.Dispatch(clientCtx, &transport.Link{Reader: inR, Writer: outW}) {
							t.Fatal("mux rejected a stream")
						}
						payload := bytes.Repeat([]byte{byte(i + 1)}, 1024)
						b := buf.New()
						_, _ = b.Write(payload)
						if err := inW.WriteMultiBuffer(buf.MultiBuffer{b}); err != nil {
							t.Fatal(err)
						}
						response, err := outR.ReadMultiBufferTimeout(3 * time.Second)
						if err != nil {
							t.Fatalf("mux response: %v", err)
						}
						defer buf.ReleaseMulti(response)
						var got bytes.Buffer
						for _, b := range response {
							_, _ = got.Write(b.Bytes())
						}
						if !bytes.Equal(got.Bytes(), payload) {
							t.Fatal("mux mixed or corrupted stream payloads")
						}
					})
				}
			})
		})
	}
}
