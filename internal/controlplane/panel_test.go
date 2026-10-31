package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kirakirai8023/Xboard-Node/internal/config"
	panelapi "github.com/kirakirai8023/Xboard-Node/internal/panel"
)

func TestPanelReconciliation_FullNotificationQueueRetainsRequest(t *testing.T) {
	cp := &PanelControlPlane{}
	statuses := make(chan StatusChange, 1)
	statuses <- StatusChange{Connected: true}
	cp.notifyResync(statuses)
	if !cp.ConsumeResyncRequest() || cp.ConsumeResyncRequest() {
		t.Fatal("a dropped notification must remain pending until consumed")
	}
	<-statuses
	cp.notifyResync(statuses)
	if !((<-statuses).NeedsResync) || cp.ConsumeResyncRequest() {
		t.Fatal("a successfully queued notification must not request a duplicate reconciliation")
	}
}

func TestPanelSnapshot_RetriesPartialFailureAndInvalidation(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(map[bool]string{false: "node", true: "machine"}[machine], func(t *testing.T) {
			var configCalls, userCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/config") {
					configCalls.Add(1)
					if r.Header.Get("If-None-Match") == `"v2"` {
						w.WriteHeader(http.StatusNotModified)
						return
					}
					w.Header().Set("ETag", `"v2"`)
					_, _ = w.Write([]byte(`{"protocol":"vless","server_port":1443}`))
					return
				}
				if userCalls.Add(1) == 1 {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"users":[{"id":1,"uuid":"11111111-1111-1111-1111-111111111111"}]}`))
			}))
			defer server.Close()
			panelCfg := config.PanelConfig{URL: server.URL, Token: "test", NodeID: 1}
			var source interface {
				Source
				SnapshotInvalidator
			}
			if machine {
				panelCfg.MachineID = 1
				source = NewMachinePanelControlPlane(panelapi.NewClient(panelCfg), config.KernelConfig{Type: "singbox"}, nil, nil)
			} else {
				source = NewPanelControlPlane(panelCfg, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
			}
			if _, err := source.Poll(context.Background()); err == nil {
				t.Fatal("expected partial poll failure")
			}
			snapshot, err := source.Poll(context.Background())
			if err != nil || snapshot.Config == nil || snapshot.Config.ServerPort != 1443 {
				t.Fatalf("config was lost after partial failure: %v, %v", snapshot, err)
			}
			snapshot, err = source.Poll(context.Background())
			if err != nil || snapshot.Config != nil {
				t.Fatalf("unchanged poll should use ETag: %v, %v", snapshot, err)
			}
			source.InvalidateSnapshot()
			snapshot, err = source.Poll(context.Background())
			if err != nil || snapshot.Config == nil || configCalls.Load() != 4 {
				t.Fatalf("forced reconciliation did not fetch full config: %v, %v", snapshot, err)
			}
		})
	}
}

func TestPanelControlPlaneInitialRejectsInvalidCustomOutbounds(t *testing.T) {
	server := newPanelTestServer(`{"protocol":"shadowsocks","server_port":8388,"custom_outbounds":[{"tag":"proxy","protocol":"socks","proxy_tag":"missing","settings":{"server":"2.2.2.2","server_port":1080}}]}`)
	defer server.Close()

	cp := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	_, err := cp.Initial(context.Background(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `initial config normalize: validate custom outbounds: custom_outbounds[0].proxy_tag references unknown outbound "missing"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPanelControlPlanePollRejectsInvalidCustomOutbounds(t *testing.T) {
	server := newPanelTestServer(`{"protocol":"shadowsocks","server_port":8388,"custom_outbounds":[{"tag":"proxy","protocol":"socks","proxy_tag":"missing","settings":{"server":"2.2.2.2","server_port":1080}}]}`)
	defer server.Close()

	cp := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	_, err := cp.Poll(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `poll config normalize: validate custom outbounds: custom_outbounds[0].proxy_tag references unknown outbound "missing"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTranslateWSEventRejectsInvalidCustomOutbounds(t *testing.T) {
	_, err := TranslateWSEvent(panelapi.WSEvent{
		Type: panelapi.WSEventSyncConfig,
		Config: &panelapi.NodeConfig{
			Protocol:   "shadowsocks",
			ServerPort: 8388,
			CustomOutbounds: []panelapi.OutboundConfig{
				{Tag: "proxy", Protocol: "socks", ProxyTag: "missing", Settings: map[string]any{"server": "2.2.2.2", "server_port": 1080}},
			},
		},
	}, config.KernelConfig{Type: "singbox"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `translate node config: validate custom outbounds: custom_outbounds[0].proxy_tag references unknown outbound "missing"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func newPanelTestServer(configBody string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/server/handshake", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"websocket":{"enabled":false},"settings":{"push_interval":60,"pull_interval":60}}`))
	})
	mux.HandleFunc("/api/v1/server/UniProxy/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(configBody))
	})
	mux.HandleFunc("/api/v1/server/UniProxy/user", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"id":1,"uuid":"11111111-1111-1111-1111-111111111111"}]}`))
	})
	return httptest.NewServer(mux)
}

func TestTranslateWSEventRejectsUnsupportedProtocolForKernel(t *testing.T) {
	_, err := TranslateWSEvent(panelapi.WSEvent{
		Type: panelapi.WSEventSyncConfig,
		Config: &panelapi.NodeConfig{
			Protocol:   "shadowsocks",
			ServerPort: 8388,
			CustomOutbounds: []panelapi.OutboundConfig{
				{Tag: "hy2", Protocol: "hysteria2", Settings: map[string]any{"server": "2.2.2.2", "server_port": 8443}},
			},
		},
	}, config.KernelConfig{Type: "xray"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), `translate node config: validate custom outbounds: custom_outbounds[0].protocol "hysteria2" is not supported by kernel "xray"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}
