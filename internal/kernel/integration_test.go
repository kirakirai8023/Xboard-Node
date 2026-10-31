// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package kernel_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kirakirai8023/Xboard-Node/internal/cert"
	"github.com/kirakirai8023/Xboard-Node/internal/config"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel/singbox"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel/xray"
	"github.com/kirakirai8023/Xboard-Node/internal/model"
	xrayCore "github.com/xtls/xray-core/core"
	xraySerial "github.com/xtls/xray-core/infra/conf/serial"
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
			active := conn
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

			// Xray SOCKS is only a test transport: the daemon's Xray Protocols()
			// excludes it, and it has no live UserManager for credential updates.
			if backend == "singbox" || tc.protocol == "vless" {
				_ = conn.Close()
				previous := traffic[1]
				users = []model.UserSpec{{ID: 1, UUID: "22222222-2222-2222-2222-222222222222", SpeedLimit: 16}}
				if _, _, err := nodeKernel.UpdateUsers(users); err != nil {
					t.Fatalf("replace credentials on the same port: %v", err)
				}
				next := dialNode(t, tc.protocol, address, echo.Addr().String(), users[0].UUID)
				defer next.Close()
				active = next
				roundTrip(t, next)
				traffic = awaitTraffic(t, nodeKernel, func(traffic map[int][2]int64) bool { return traffic[1][0] > previous[0] && traffic[1][1] > previous[1] })
				if traffic[1][0] <= previous[0] || traffic[1][1] <= previous[1] {
					t.Fatalf("replacement reset traffic counters: before=%v after=%v", previous, traffic[1])
				}
				assertCredentialRejected(t, tc.protocol, address, echo.Addr().String(), "11111111-1111-1111-1111-111111111111")
			}

			// Revoking the last user calls Stop. It must close real listeners
			// and existing sessions, rather than just clearing IsRunning().
			nodeKernel.Stop()
			if nodeKernel.IsRunning() {
				t.Fatal("kernel still reports running after Stop")
			}
			if probe, err := net.DialTimeout("tcp", address, time.Second); err == nil {
				probe.Close()
				t.Fatal("stopped kernel still accepts new connections")
			}
			_ = active.SetDeadline(time.Now().Add(time.Second))
			if _, err := active.Write([]byte("revoked")); err == nil {
				var response [7]byte
				if n, _ := active.Read(response[:]); n > 0 {
					t.Fatal("stopped kernel still proxies existing sessions")
				}
			}
			if err := nodeKernel.Start(node, users, kernel.TLSCert{}); err != nil {
				t.Fatalf("restart after all users revoked: %v", err)
			}
			restored := dialNode(t, tc.protocol, address, echo.Addr().String(), users[0].UUID)
			defer restored.Close()
			roundTrip(t, restored)
		})
	}
}

func awaitTraffic(t *testing.T, node kernel.Kernel, ready func(map[int][2]int64) bool) map[int][2]int64 {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		traffic, _, _, err := node.GetUserTraffic(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ready(traffic) {
			return traffic
		}
		if time.Now().After(deadline) {
			t.Fatalf("traffic counters did not settle: %v", traffic)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestXray_PrivateDestinationsRequireExplicitOutboundPermission(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("private-target"))
	}))
	defer target.Close()
	_, targetPort, _ := net.SplitHostPort(target.Listener.Addr().String())
	portNumber, _ := strconv.Atoi(targetPort)
	for _, allow := range []bool{false, true} {
		name := "default_block"
		if allow {
			name = "explicit_allow"
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.KernelConfig{Type: "xray", LogLevel: "error", ConfigDir: t.TempDir()}
			if allow {
				cfg.CustomOutbound = []map[string]any{{"tag": "direct", "protocol": "freedom",
					"settings": map[string]any{"finalRules": []map[string]any{{
						"action": "allow", "ip": []string{"127.0.0.1/32", "::1/128"}, "port": portNumber,
					}}}}}
			}
			nodeKernel := xray.New(cfg)
			node := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: unusedPort(t),
				// Deliberately bypass router blocking: the freedom outbound must
				// enforce its own final rules for IP and domain destinations.
				CustomRouteRules: []model.CustomRouteRule{
					{Match: model.RouteMatch{IPCIDRs: []string{"127.0.0.0/8", "::1/128"}}, Action: model.RouteAction{Type: "direct"}},
					{Match: model.RouteMatch{Domains: []string{"localhost"}}, Action: model.RouteAction{Type: "direct"}},
				}}
			users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}}
			if err := nodeKernel.Start(node, users, kernel.TLSCert{}); err != nil {
				t.Fatal(err)
			}
			defer nodeKernel.Stop()
			address := net.JoinHostPort("127.0.0.1", strconv.Itoa(node.ServerPort))
			for _, host := range []string{"127.0.0.1", "localhost"} {
				before := hits.Load()
				conn := dialNode(t, "vless", address, net.JoinHostPort(host, targetPort), users[0].UUID)
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				_, _ = io.WriteString(conn, "GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")
				response, err := io.ReadAll(conn)
				conn.Close()
				if allow {
					if err != nil || !bytes.Contains(response, []byte("private-target")) || hits.Load() != before+1 {
						t.Fatalf("explicit permission failed for %s: response=%q, error=%v", host, response, err)
					}
				} else if len(response) != 0 || hits.Load() != before {
					t.Fatalf("private destination reached through %s", host)
				}
			}
		})
	}
}

func TestKernel_TLSCertificateReloadSamePort(t *testing.T) {
	material := func() kernel.TLSCert {
		mgr := cert.NewManager(config.CertConfig{CertMode: "self", CertDir: t.TempDir(), Domain: "localhost"})
		if err := mgr.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return mgr.TLSCert()
	}
	first, renewed := material(), material()
	for _, backend := range []string{"singbox", "xray"} {
		t.Run(backend, func(t *testing.T) {
			cfg := config.KernelConfig{Type: backend, LogLevel: "error", ConfigDir: t.TempDir()}
			var nodeKernel kernel.Kernel
			if backend == "singbox" {
				nodeKernel = singbox.New(cfg)
			} else {
				nodeKernel = xray.New(cfg)
			}
			node := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: unusedPort(t), TLS: 1}
			users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}}
			if err := nodeKernel.Start(node, users, first); err != nil {
				t.Fatal(err)
			}
			defer nodeKernel.Stop()
			assertCertificate := func(want kernel.TLSCert) {
				t.Helper()
				conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp",
					net.JoinHostPort("127.0.0.1", strconv.Itoa(node.ServerPort)), &tls.Config{InsecureSkipVerify: true})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				block, _ := pem.Decode(want.CertPEM)
				if block == nil || !bytes.Equal(conn.ConnectionState().PeerCertificates[0].Raw, block.Bytes) {
					t.Fatal("listener is serving outdated certificate material")
				}
			}
			assertCertificate(first)
			if err := nodeKernel.Reload(node, users, renewed); err != nil {
				t.Fatalf("renew certificate on occupied port: %v", err)
			}
			assertCertificate(renewed)
			if err := nodeKernel.Reload(node, users, kernel.TLSCert{CertPEM: []byte("invalid"), KeyPEM: []byte("invalid")}); err == nil {
				t.Fatal("invalid renewal unexpectedly succeeded")
			}
			assertCertificate(renewed) // A failed renewal must restore the last working certificate.
			if backend == "xray" {
				invalidNode := *node
				invalidNode.Network = "invalid-transport"
				if err := nodeKernel.Reload(&invalidNode, users, renewed); err == nil {
					t.Fatal("invalid replacement unexpectedly succeeded")
				}
				assertCertificate(renewed) // Also exercise rollback after releasing the old listener.
			}
		})
	}
}

// Use a local TLS 1.3 target both for REALITY camouflage and HTTPS payloads.
// No public DNS, external servers, production credentials or fixed ports.
func TestXray_RealityVisionUnlimited(t *testing.T) {
	payload := bytes.Repeat([]byte("vision-transfer\n"), 128*1024)
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	target.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13}
	target.StartTLS()
	defer target.Close()
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverPort, clientPort := unusedPort(t), unusedPort(t)
	targetAddress := target.Listener.Addr().String()
	_, targetPortText, _ := net.SplitHostPort(targetAddress)
	targetPort, _ := strconv.Atoi(targetPortText)
	for _, mode := range []string{"auto", "compatible"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config.KernelConfig{Type: "xray", LogLevel: "error", ConfigDir: t.TempDir(), XrayForwarding: mode,
				CustomOutbound: []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{"finalRules": []map[string]any{{"action": "allow", "ip": []string{"127.0.0.1/32"}, "port": targetPort}}}}}}
			server := xray.New(cfg)
			user := model.UserSpec{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}
			node := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: serverPort, Network: "tcp", TLS: 2, Flow: "xtls-rprx-vision",
				TLSSettings:      map[string]any{"private_key": base64.RawURLEncoding.EncodeToString(private.Bytes()), "short_id": "0123456789abcdef", "server_name": "example.com", "dest": targetAddress},
				CustomRouteRules: []model.CustomRouteRule{{Match: model.RouteMatch{IPCIDRs: []string{"127.0.0.1/32"}}, Action: model.RouteAction{Type: "direct"}}}}
			if err := server.Start(node, []model.UserSpec{user}, kernel.TLSCert{}); err != nil {
				t.Fatal(err)
			}
			defer server.Stop()
			clientConfig := map[string]any{"log": map[string]any{"loglevel": "error"},
				"inbounds": []map[string]any{{"listen": "127.0.0.1", "port": clientPort, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}}},
				"outbounds": []map[string]any{{"protocol": "vless", "settings": map[string]any{"vnext": []map[string]any{{"address": "127.0.0.1", "port": serverPort, "users": []map[string]any{{"id": user.UUID, "flow": "xtls-rprx-vision", "encryption": "none"}}}}},
					"streamSettings": map[string]any{"network": "tcp", "security": "reality", "realitySettings": map[string]any{"serverName": "example.com", "fingerprint": "chrome", "password": base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()), "shortId": "0123456789abcdef"}}}}}
			data, _ := json.Marshal(clientConfig)
			protobuf, err := xraySerial.LoadJSONConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			client, err := xrayCore.New(protobuf)
			if err != nil {
				t.Fatal(err)
			}
			if err = client.Start(); err != nil {
				client.Close()
				t.Fatal(err)
			}
			defer client.Close()
			dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(clientPort)), nil, &net.Dialer{Timeout: 3 * time.Second})
			if err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true,
				DialContext: dialer.(proxy.ContextDialer).DialContext}
			defer transport.CloseIdleConnections()
			httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
			response, err := httpClient.Get(target.URL)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("Vision payload length=%d, error=%v", len(got), err)
			}
			awaitTraffic(t, server, func(traffic map[int][2]int64) bool { return traffic[1][0] > 0 && traffic[1][1] >= int64(len(payload)) })
		})
	}
}

func unusedPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func dialNode(t *testing.T, protocol, address, target, credential string) net.Conn {
	t.Helper()
	conn, err := dialNodeWithCredential(protocol, address, target, credential)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func dialNodeWithCredential(protocol, address, target, credential string) (net.Conn, error) {
	if protocol == "socks" {
		return dialSOCKS(address, target, credential)
	}
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		return nil, err
	}
	id, err := hex.DecodeString(strings.ReplaceAll(credential, "-", ""))
	if err != nil {
		conn.Close()
		return nil, err
	}
	host, portString, _ := net.SplitHostPort(target)
	port, _ := strconv.Atoi(portString)
	header := append([]byte{0}, id...)
	// VLESS v0: no addons, TCP command, destination port, typed address.
	header = append(header, 0, 1, byte(port>>8), byte(port))
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			header = append(header, 1)
			header = append(header, ipv4...)
		} else {
			header = append(header, 3)
			header = append(header, ip.To16()...)
		}
	} else {
		header = append(header, 2, byte(len(host)))
		header = append(header, host...)
	}
	if _, err := conn.Write(header); err != nil {
		conn.Close()
		return nil, err
	}
	return &vlessTestConn{Conn: conn}, nil
}

func assertCredentialRejected(t *testing.T, protocol, address, target, credential string) {
	t.Helper()
	conn, err := dialNodeWithCredential(protocol, address, target, credential)
	if err != nil {
		return // SOCKS authentication can fail during dialing.
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("revoked")); err != nil {
		return
	}
	var response [7]byte
	if n, _ := conn.Read(response[:]); n > 0 {
		t.Fatal("revoked credentials still allowed proxy traffic")
	}
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

func dialSOCKS(address, target, credential string) (net.Conn, error) {
	dialer, err := proxy.SOCKS5("tcp", address, &proxy.Auth{User: credential, Password: credential}, &net.Dialer{Timeout: 2 * time.Second})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", target)
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
