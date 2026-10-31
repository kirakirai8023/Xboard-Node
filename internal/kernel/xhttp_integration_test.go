// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration

package kernel_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kirakirai8023/Xboard-Node/internal/cert"
	"github.com/kirakirai8023/Xboard-Node/internal/config"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel/xray"
	"github.com/kirakirai8023/Xboard-Node/internal/model"
	xrayCore "github.com/xtls/xray-core/core"
	xraySerial "github.com/xtls/xray-core/infra/conf/serial"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// Terminate TLS at an HTTP reverse proxy, then forward XHTTP to the node.
// This exercises the CDN topology locally, without a public CDN or credentials.
func TestXray_XHTTPThroughCDN(t *testing.T) {
	for _, originTLS := range []bool{false, true} {
		for _, mode := range []string{"packet-up", "stream-up"} {
			t.Run(strconv.FormatBool(originTLS)+"/"+mode, func(t *testing.T) {
				payload := bytes.Repeat([]byte("xhttp-upload-and-download\n"), 4096)
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, "read body", http.StatusBadRequest)
						return
					}
					_, _ = w.Write(data)
				}))
				defer target.Close()
				_, targetPortText, _ := net.SplitHostPort(target.Listener.Addr().String())
				targetPort, _ := strconv.Atoi(targetPortText)
				cfg := config.KernelConfig{Type: "xray", LogLevel: "error", ConfigDir: t.TempDir(),
					CustomOutbound: []map[string]any{{"tag": "direct", "protocol": "freedom", "settings": map[string]any{
						"finalRules": []map[string]any{{"action": "allow", "ip": []string{"127.0.0.1/32"}, "port": targetPort}},
					}}}}
				server := xray.New(cfg)
				user := model.UserSpec{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}
				newNode := func() *model.NodeSpec {
					return &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", Network: "xhttp",
						NetworkSettings:  map[string]any{"path": "/transport", "host": "cdn.example", "mode": "auto", "extra": map[string]any{"headers": []any{}, "xmux": []any{}}},
						CustomRouteRules: []model.CustomRouteRule{{Match: model.RouteMatch{IPCIDRs: []string{"127.0.0.1/32"}}, Action: model.RouteAction{Type: "direct"}}}}
				}
				node := newNode()
				node.ServerPort = unusedPort(t)
				var material kernel.TLSCert
				scheme := "http"
				if originTLS {
					node.TLS = 1
					scheme = "https"
					manager := cert.NewManager(config.CertConfig{CertMode: "self", CertDir: t.TempDir(), Domain: "localhost"})
					if err := manager.Start(context.Background()); err != nil {
						t.Fatal(err)
					}
					material = manager.TLSCert()
				}
				hash := kernel.ComputeHash(node, []model.UserSpec{user})
				if err := server.Start(node, []model.UserSpec{user}, material); err != nil {
					t.Fatal(err)
				}
				defer server.Stop()
				if kernel.ComputeHash(node, []model.UserSpec{user}) != hash {
					t.Fatal("starting XHTTP modified the panel snapshot")
				}
				origin, _ := url.Parse(scheme + "://127.0.0.1:" + strconv.Itoa(node.ServerPort))
				reverse := httputil.NewSingleHostReverseProxy(origin)
				reverse.FlushInterval = -1
				originTransport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, ForceAttemptHTTP2: mode == "stream-up"}
				defer originTransport.CloseIdleConnections()
				reverse.Transport = originTransport
				if mode == "stream-up" && !originTLS {
					h2cTransport := &http2.Transport{AllowHTTP: true, DialTLSContext: func(ctx context.Context, network, address string, _ *tls.Config) (net.Conn, error) {
						return (&net.Dialer{}).DialContext(ctx, network, address)
					}}
					defer h2cTransport.CloseIdleConnections()
					reverse.Transport = h2cTransport
				}
				var requests atomic.Int32
				cdn := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.TLS == nil || r.Host != "cdn.example" {
						t.Error("CDN did not receive the expected HTTPS host")
					}
					requests.Add(1)
					reverse.ServeHTTP(w, r)
				}))
				cdn.EnableHTTP2 = true
				cdn.StartTLS()
				defer cdn.Close()
				_, cdnPortText, _ := net.SplitHostPort(cdn.Listener.Addr().String())
				cdnPort, _ := strconv.Atoi(cdnPortText)
				cdnFingerprint := sha256.Sum256(cdn.Certificate().Raw)
				clientPort := unusedPort(t)
				clientConfig := map[string]any{
					"log":      map[string]any{"loglevel": "error"},
					"inbounds": []map[string]any{{"listen": "127.0.0.1", "port": clientPort, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}}},
					"outbounds": []map[string]any{{"protocol": "vless", "settings": map[string]any{"vnext": []map[string]any{
						{"address": "127.0.0.1", "port": cdnPort, "users": []map[string]any{{"id": user.UUID, "encryption": "none"}}},
					}}, "streamSettings": map[string]any{
						"network": "xhttp", "security": "tls", "tlsSettings": map[string]any{"pinnedPeerCertSha256": hex.EncodeToString(cdnFingerprint[:]), "serverName": "cdn.example", "alpn": []string{"h2"}},
						"xhttpSettings": map[string]any{"path": "/transport", "host": "cdn.example", "mode": mode},
					}}},
				}
				data, _ := json.Marshal(clientConfig)
				pb, err := xraySerial.LoadJSONConfig(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				client, err := xrayCore.New(pb)
				if err != nil {
					t.Fatal(err)
				}
				if err := client.Start(); err != nil {
					client.Close()
					t.Fatal(err)
				}
				defer client.Close()
				dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(clientPort)), nil, &net.Dialer{Timeout: 3 * time.Second})
				if err != nil {
					t.Fatal(err)
				}
				transport := &http.Transport{DialContext: dialer.(proxy.ContextDialer).DialContext}
				defer transport.CloseIdleConnections()
				httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
				transfer := func() {
					t.Helper()
					response, err := httpClient.Post(target.URL, "application/octet-stream", bytes.NewReader(payload))
					if err != nil {
						t.Fatal(err)
					}
					got, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil || !bytes.Equal(got, payload) {
						t.Fatalf("XHTTP echoed %d/%d bytes: %v", len(got), len(payload), err)
					}
				}
				transfer()
				// Fresh panel data includes PHP-style empty arrays on every sync.
				fresh := newNode()
				fresh.ServerPort, fresh.TLS = node.ServerPort, node.TLS
				if err := server.Reload(fresh, []model.UserSpec{user}, material); err != nil {
					t.Fatalf("reload identical XHTTP settings: %v", err)
				}
				transfer()
				if requests.Load() == 0 {
					t.Fatal("traffic bypassed the simulated CDN")
				}
				awaitTraffic(t, server, func(traffic map[int][2]int64) bool {
					return traffic[1][0] >= int64(len(payload))*2 && traffic[1][1] >= int64(len(payload))*2
				})
			})
		}
	}
}
