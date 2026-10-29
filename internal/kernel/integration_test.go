// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package kernel_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kirakirai8023/Xboard-Node/internal/config"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel/singbox"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel/xray"
	"github.com/kirakirai8023/Xboard-Node/internal/model"
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"
)

// Exercise the official libraries with real authenticated, bidirectional traffic.
// Map-only configuration tests cannot detect removed options or lifecycle APIs.
func TestOfficialKernelsTraffic(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	for _, tc := range []struct{ backend, protocol string }{
		{"singbox", "socks"}, {"singbox", "vless"},
		{"xray", "socks"}, {"xray", "vless"},
	} {
		t.Run(tc.backend+"/"+tc.protocol, func(t *testing.T) {
			backend := tc.backend
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := listener.Addr().(*net.TCPAddr).Port
			_ = listener.Close()
			cfg := config.KernelConfig{Type: backend, LogLevel: "error", ConfigDir: t.TempDir()}
			if backend == "xray" {
				// New Xray releases block private destinations inside freedom,
				// in addition to routing rules. Allow only this test echo server.
				cfg.CustomOutbound = []map[string]any{{
					"tag": "direct", "protocol": "freedom",
					"settings": map[string]any{"finalRules": []map[string]any{{
						"action": "allow", "ip": []string{"127.0.0.1/32"},
						"port": echo.Addr().(*net.TCPAddr).Port,
					}}},
				}}
			}
			var nodeKernel kernel.Kernel
			if backend == "singbox" {
				nodeKernel = singbox.New(cfg)
			} else {
				nodeKernel = xray.New(cfg)
			}
			shared := rate.NewLimiter(1<<20, 1<<20)
			nodeKernel.(interface {
				SetSpeedLimitFunc(func(string) *rate.Limiter)
			}).SetSpeedLimitFunc(func(string) *rate.Limiter {
				return shared
			})
			node := &model.NodeSpec{
				Protocol: tc.protocol, ListenIP: "127.0.0.1", ServerPort: port,
				CustomRouteRules: []model.CustomRouteRule{{
					Match:  model.RouteMatch{IPCIDRs: []string{"127.0.0.0/8"}},
					Action: model.RouteAction{Type: "direct"},
				}},
			}
			users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111", SpeedLimit: 8}}
			if err := nodeKernel.Start(node, users, kernel.TLSCert{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(nodeKernel.Stop)
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			conn := dialNode(t, tc.protocol, address, echo.Addr().String(), users[0].UUID)
			defer conn.Close()
			roundTrip(t, conn)

			users[0].SpeedLimit = 16
			if _, _, err := nodeKernel.UpdateUsers(users); err != nil {
				t.Fatal(err)
			}
			roundTrip(t, conn) // Limit-only updates must preserve this connection.
			traffic, _, _, err := nodeKernel.GetUserTraffic(context.Background())
			if err != nil || traffic[1][0] < 32768 || traffic[1][1] < 32768 {
				t.Fatalf("bidirectional traffic = %v, error = %v", traffic, err)
			}

			if backend == "singbox" {
				_ = conn.Close()
				previous := traffic[1]
				users = []model.UserSpec{{ID: 1, UUID: "22222222-2222-2222-2222-222222222222", SpeedLimit: 16}}
				if _, _, err := nodeKernel.UpdateUsers(users); err != nil {
					t.Fatalf("replace credentials on the same port: %v", err)
				}
				next := dialNode(t, tc.protocol, address, echo.Addr().String(), users[0].UUID)
				defer next.Close()
				roundTrip(t, next)
				traffic, _, _, _ = nodeKernel.GetUserTraffic(context.Background())
				if traffic[1][0] <= previous[0] || traffic[1][1] <= previous[1] {
					t.Fatalf("replacement reset traffic counters: before=%v after=%v", previous, traffic[1])
				}
			}
		})
	}
}

func dialNode(t *testing.T, protocol, address, target, credential string) net.Conn {
	t.Helper()
	if protocol == "socks" {
		return dialSOCKS(t, address, target, credential)
	}
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	id, err := hex.DecodeString(strings.ReplaceAll(credential, "-", ""))
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	host, portString, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portString)
	header := append([]byte{0}, id...)
	// VLESS v0: no addons, TCP command, destination port, IPv4 address.
	header = append(header, 0, 1, byte(port>>8), byte(port), 1)
	header = append(header, net.ParseIP(host).To4()...)
	if _, err := conn.Write(header); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	return &vlessTestConn{Conn: conn}
}

type vlessTestConn struct {
	net.Conn
	readHeader bool
}

func (c *vlessTestConn) Read(p []byte) (int, error) {
	if !c.readHeader {
		var header [2]byte
		if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
			return 0, err
		}
		if _, err := io.CopyN(io.Discard, c.Conn, int64(header[1])); err != nil {
			return 0, err
		}
		c.readHeader = true
	}
	return c.Conn.Read(p)
}

func dialSOCKS(t *testing.T, address, target, credential string) net.Conn {
	t.Helper()
	dialer, err := proxy.SOCKS5("tcp", address, &proxy.Auth{User: credential, Password: credential}, &net.Dialer{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", target)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func roundTrip(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	want := bytes.Repeat([]byte("x"), 16384)
	if _, err := conn.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, got) {
		t.Fatal("proxied payload differs")
	}
}
