// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kirakirai8023/Xboard-Node/internal/config"
	"github.com/kirakirai8023/Xboard-Node/internal/model"
	"github.com/kirakirai8023/Xboard-Node/internal/panel"
)

func TestDeployment_Topologies(t *testing.T) {
	for _, tc := range []struct {
		name, network, topology string
		tls, port               int
		wantError               string
	}{
		{"cloudflare_xhttp", "xhttp", "cloudflare", 1, 443, ""},
		{"cloudflare_ws", "ws", "cloudflare", 1, 8443, ""},
		{"proxy_plain_backend", "xhttp", "proxy", 0, 8080, ""},
		{"reality_direct", "tcp", "direct", 2, 443, ""},
		{"reality_cdn", "xhttp", "cloudflare", 2, 443, "cannot pass through REALITY"},
		{"raw_tcp_cdn", "tcp", "cloudflare", 1, 443, "HTTP transport"},
		{"plaintext_https_origin", "xhttp", "cloudflare", 0, 443, "TLS enabled"},
		{"nonstandard_cloudflare_port", "xhttp", "cloudflare", 1, 9443, "origin rule"},
		{"custom_origin_port", "xhttp", "cdn", 1, 9443, ""},
		{"invalid_port", "xhttp", "direct", 1, 0, "port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &model.NodeSpec{Protocol: "vless", Network: tc.network, TLS: tc.tls, ServerPort: tc.port,
				TLSSettings: map[string]any{"private_key": "private-fixture", "server_name": "example.com"}}
			_, err := inspectDeploymentNode(node, 1, config.CertConfig{}, tc.topology)
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("expected %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestDeployment_PanelPreflightAndSafeReport(t *testing.T) {
	for _, tc := range []struct {
		name, flow, kernel string
		status             int
		wantError          string
	}{
		{"valid", "", "xray", 200, ""},
		{"vision_xhttp", "xtls-rprx-vision", "xray", 200, "clear flow"},
		{"wrong_kernel", "", "singbox", 200, "use xray"},
		{"authentication_failure", "", "xray", 403, "could not read node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("token") != "panel-fixture-secret" {
					t.Error("missing panel authentication")
				}
				if strings.HasSuffix(r.URL.Path, "/config") {
					w.WriteHeader(tc.status)
					_ = json.NewEncoder(w).Encode(panel.NodeConfig{Protocol: "vless", Network: "xhttp", ServerPort: 443, TLS: 1, Flow: tc.flow,
						ServerName: "node.example.com", NetworkSettings: map[string]any{"path": "/fixture", "mode": "packet-up"}})
				} else {
					_, _ = w.Write([]byte(`{"users":[]}`))
				}
			}))
			defer server.Close()
			cfg := config.Config{Panel: config.PanelConfig{URL: server.URL, Token: "panel-fixture-secret", NodeID: 5, NodeType: "vless"}, Kernel: config.KernelConfig{Type: tc.kernel}}
			report, err := inspectDeployment([]config.Config{cfg}, "cloudflare")
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) || strings.Contains(err.Error(), "panel-fixture-secret") {
					t.Fatalf("unsafe or incorrect error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Nodes) != 1 || report.Nodes[0].Users != 0 || len(report.Warnings) != 1 {
				t.Fatalf("missing no-user warning: %+v", report)
			}
			for _, format := range []string{"json", "text", "tsv"} {
				var out bytes.Buffer
				if err := writeDeploymentReport(&out, report, format); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(out.String(), "panel-fixture-secret") {
					t.Fatal("report leaked a credential")
				}
				if format == "tsv" && !strings.Contains(out.String(), "\tfalse\ttrue\t0") {
					t.Fatal("missing certificate and user fields")
				}
			}
		})
	}
}

func TestDeployment_MachineNodesAndPortCollision(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     int
		collide   bool
		wantError bool
	}{
		{"empty", 0, false, false}, {"mixed_nodes", 2, false, false}, {"collision", 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v2/server/machine/nodes":
					if r.Method != http.MethodPost {
						t.Error("wrong machine method")
					}
					nodes := []panel.MachineNode{}
					for i := 1; i <= tc.count; i++ {
						nodes = append(nodes, panel.MachineNode{ID: i, Type: "vless"})
					}
					_ = json.NewEncoder(w).Encode(panel.MachineNodesResponse{Nodes: nodes})
				case "/api/v2/server/config":
					port := 443
					if !tc.collide && r.URL.Query().Get("node_id") == "2" {
						port = 8443
					}
					_ = json.NewEncoder(w).Encode(panel.NodeConfig{Protocol: "vless", Network: "xhttp", TLS: 1, ServerPort: port})
				case "/api/v2/server/user":
					_, _ = w.Write([]byte(`{"users":[{"id":1,"uuid":"fixture-user"}]}`))
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg := config.Config{Panel: config.PanelConfig{URL: server.URL}, Machine: &config.MachineConfig{MachineID: 1, Token: "fixture-machine"}, Kernel: config.KernelConfig{Type: "singbox"}}
			report, err := inspectDeployment([]config.Config{cfg}, "cloudflare")
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "same listener") {
					t.Fatalf("expected port collision, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Nodes) != tc.count {
				t.Fatalf("unexpected node count: %+v", report)
			}
			if tc.count == 0 && len(report.Warnings) != 1 {
				t.Fatal("missing empty machine warning")
			}
		})
	}
}

func deploymentPEM(t *testing.T, expired bool) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node.example.com"}, DNSNames: []string{"node.example.com"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if expired {
		leaf.NotAfter = now.Add(-time.Minute)
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
}

func TestDeployment_CertificateValidation(t *testing.T) {
	cert, key := deploymentPEM(t, false)
	valid := config.CertConfig{CertMode: "content", Domain: "node.example.com", CertContent: cert, KeyContent: key}
	if err := validateDeploymentCert(valid); err != nil {
		t.Fatal(err)
	}
	wrong := valid
	wrong.Domain = "other.example.com"
	if err := validateDeploymentCert(wrong); err == nil {
		t.Fatal("accepted wrong hostname")
	}
	wrong = valid
	wrong.CertContent, wrong.KeyContent = deploymentPEM(t, true)
	if err := validateDeploymentCert(wrong); err == nil {
		t.Fatal("accepted expired certificate")
	}
	wrong = valid
	_, wrong.KeyContent = deploymentPEM(t, false)
	if err := validateDeploymentCert(wrong); err == nil || strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Fatal("invalid key was not rejected safely")
	}
	node := &model.NodeSpec{Protocol: "vless", Network: "xhttp", TLS: 1, ServerPort: 443, CertConfig: &config.CertConfig{CertMode: "none"}}
	view, err := inspectDeploymentNode(node, 1, valid, "cloudflare")
	if err != nil || !view.PanelCert || view.CertMode != "none" || !view.NeedsCert {
		t.Fatalf("missing panel certificate must reach the wizard without silently using local material: %+v %v", view, err)
	}
	valid.Source = config.CertSourceLocal
	view, err = inspectDeploymentNode(node, 1, valid, "cloudflare")
	if err != nil || view.PanelCert || view.CertMode != "content" {
		t.Fatalf("explicit local policy was rejected: %+v %v", view, err)
	}
	valid.Source = ""
	node.CertConfig = &valid
	view, err = inspectDeploymentNode(node, 1, config.CertConfig{CertMode: "file", CertFile: "missing"}, "cloudflare")
	if err != nil || !view.PanelCert {
		t.Fatalf("panel precedence failed: %v", err)
	}
}

func TestDeployment_PrivateDNSProfileAndConfigMerge(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "dns-token")
	profile := filepath.Join(dir, "certificate.yml")
	output := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(token, []byte("dns-fixture-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runDeploymentCert([]string{"--mode", "dns", "--domain", "node.example.com", "--dns-token-file", token, "--output", profile}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(profile)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("profile permissions must be private")
		}
	}
	args := []string{"--mode", "node", "--panel-url", "https://panel.example.com", "--node-id", "1", "--kernel", "xray", "--output", output}
	if err := runConfigInit(append(append([]string{}, args...), "--cert-profile", profile)); err != nil {
		t.Fatal(err)
	}
	if err := runConfigInit(append(append([]string{}, args...), "--config", output)); err != nil {
		t.Fatal(err)
	}
	root, err := loadWritableRootConfig(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Instances) != 1 || !root.Instances[0].Cert.UsesLocalSource() || root.Instances[0].Cert.DNSEnv["CLOUDFLARE_DNS_API_TOKEN"] != "dns-fixture-secret" {
		t.Fatal("certificate profile lost on merge")
	}
	if err := runConfigInit([]string{"--cert-profile"}); err == nil {
		t.Fatal("accepted missing profile value")
	}
}

func TestDeployment_PanelNoneAndLocalProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			_, _ = w.Write([]byte(`{"protocol":"vless","network":"xhttp","tls":1,"server_port":443,"server_name":"node.example.com","cert_config":{"cert_mode":"none"}}`))
		} else {
			_, _ = w.Write([]byte(`{"users":[{"id":1,"uuid":"fixture"}]}`))
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	output := filepath.Join(dir, "config.yml")
	profile := filepath.Join(dir, "certificate.yml")
	token := filepath.Join(dir, "token")
	credentials := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	// Inspecting a new binding must reach certificate selection despite the
	// panel's default none policy; installed inspection remains strict.
	if err := runDeploymentInspect([]string{"--panel-url", server.URL, "--node-id", "1", "--token-file", token, "--topology", "cloudflare"}); err != nil {
		t.Fatal(err)
	}
	args := []string{"--mode", "node", "--node-id", "1", "--panel-url", server.URL, "--kernel", "xray", "--output", output, "--credentials-out", credentials, "--token-file", token}
	if err := runConfigInit(args); err != nil {
		t.Fatal(err)
	}
	inspectArgs := []string{"--config", output, "--credentials-in", credentials, "--topology", "cloudflare"}
	if err := runDeploymentInspect(inspectArgs); err == nil {
		t.Fatal("installed node without certificate was accepted")
	}
	certPEM, keyPEM := deploymentPEM(t, false)
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, []byte(certPEM), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(keyPEM), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runDeploymentCert([]string{"--mode", "file", "--domain", "node.example.com", "--cert-file", certFile, "--key-file", keyFile, "--output", profile}); err != nil {
		t.Fatal(err)
	}
	if err := runConfigInit(append(args, "--config", output, "--cert-profile", profile)); err != nil {
		t.Fatal(err)
	}
	if err := runDeploymentInspect(inspectArgs); err != nil {
		t.Fatalf("selected local certificate was overridden by panel none: %v", err)
	}
}

func TestDeployment_CredentialsRestored(t *testing.T) {
	t.Setenv("INSTANCE_FIXTURE_API_KEY", "before")
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(path, []byte("INSTANCE_FIXTURE_API_KEY=after\nUNRELATED=ignored\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restore, err := deploymentCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("INSTANCE_FIXTURE_API_KEY") != "after" {
		t.Fatal("credential not loaded")
	}
	restore()
	if os.Getenv("INSTANCE_FIXTURE_API_KEY") != "before" {
		t.Fatal("environment not restored")
	}
}

func TestDeployment_ExistingSharedCertificate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/config") {
			_, _ = w.Write([]byte(`{"protocol":"vless","network":"xhttp","tls":1,"server_port":443,"server_name":"node.example.com"}`))
		} else {
			_, _ = w.Write([]byte(`{"users":[{"id":1,"uuid":"fixture"}]}`))
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	output := filepath.Join(dir, "config.yml")
	token := filepath.Join(dir, "token")
	credentials := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(token, []byte("fixture-panel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runConfigInit([]string{"--mode", "node", "--node-id", "1", "--panel-url", server.URL, "--kernel", "xray", "--output", output, "--credentials-out", credentials, "--token-file", token}); err != nil {
		t.Fatal(err)
	}
	root, err := loadWritableRootConfig(output)
	if err != nil {
		t.Fatal(err)
	}
	root.Config.Cert = config.CertConfig{CertMode: "dns", Domain: "node.example.com", DNSProvider: "cloudflare", DNSEnv: map[string]string{"CF_API_TOKEN": "fixture-dns"}}
	if err := writeRootConfig(output, root); err != nil {
		t.Fatal(err)
	}
	// Both installed inspection and re-binding use the shared policy through
	// the normal config loader, including its credential resolution.
	if err := runDeploymentInspect([]string{"--config", output, "--credentials-in", credentials, "--topology", "cloudflare"}); err != nil {
		t.Fatal(err)
	}
	if err := runDeploymentInspect([]string{"--existing-config", output, "--credentials-in", credentials, "--panel-url", server.URL, "--node-id", "1", "--kernel", "xray", "--token-file", token, "--topology", "cloudflare"}); err != nil {
		t.Fatal(err)
	}
	root.Config.Cert = config.CertConfig{}
	if err := writeRootConfig(output, root); err != nil {
		t.Fatal(err)
	}
	if err := runDeploymentInspect([]string{"--config", output, "--credentials-in", credentials}); err == nil || !strings.Contains(err.Error(), "requires a certificate") {
		t.Fatalf("missing certificate did not fail installed inspection: %v", err)
	}
}

func TestDeployment_ListenerAddressCollisions(t *testing.T) {
	for _, tc := range []struct {
		a, b, protocol string
		want           bool
	}{
		{"::", "127.0.0.1", "vless", true}, {"0.0.0.0", "127.0.0.1", "vless", true},
		{"127.0.0.1", "127.0.0.2", "vless", false}, {"::1", "127.0.0.1", "vless", false},
		{"::", "::", "hysteria2", false},
	} {
		a := deploymentNode{Port: 443, Listen: tc.a, Protocol: "vless"}
		b := deploymentNode{Port: 443, Listen: tc.b, Protocol: tc.protocol}
		if deploymentListenersOverlap(a, b) != tc.want {
			t.Fatalf("unexpected collision: %+v %+v", a, b)
		}
	}
}
