// SPDX-License-Identifier: AGPL-3.0-only

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/kirakirai8023/Xboard-Node/internal/cert"
	"github.com/kirakirai8023/Xboard-Node/internal/config"
	"github.com/kirakirai8023/Xboard-Node/internal/kernel"
	"github.com/kirakirai8023/Xboard-Node/internal/model"
)

func TestCertificatePolicy_LocalSurvivesPanelAndLegacyUpdates(t *testing.T) {
	ctx := context.Background()
	s := newTestService(&fakeKernel{})
	s.cfg.Cert = config.CertConfig{Source: "local", CertMode: "self", Domain: "local.example.com", CertDir: t.TempDir()}
	s.cert = cert.NewManager(s.cfg.Cert)
	if err := s.cert.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.cert.Stop()
	before := s.cert.TLSCert()
	if !before.HasCert() {
		t.Fatal("fixture certificate was not generated")
	}
	for _, node := range []*model.NodeSpec{
		{Protocol: "vless", TLS: 1, CertConfig: &config.CertConfig{CertMode: "none"}},
		{Protocol: "vless", TLS: 1, CertConfig: &config.CertConfig{CertMode: "dns", DNSProvider: "unsupported-fixture", Domain: "panel.example.com"}},
		{Protocol: "vless", TLS: 1, Domain: "legacy.example.com", AutoTLS: true, KernelLogLevel: "debug"},
	} {
		original, err := json.Marshal(node)
		if err != nil {
			t.Fatal(err)
		}
		if changed := s.applyRemoteOverrides(ctx, node); changed {
			t.Fatal("panel replaced the explicitly selected local certificate")
		}
		current := s.cert.TLSCert()
		if !bytes.Equal(current.CertPEM, before.CertPEM) || !bytes.Equal(current.KeyPEM, before.KeyPEM) || s.cfg.Cert.Domain != "local.example.com" || !s.cfg.Cert.UsesLocalSource() {
			t.Fatal("local certificate or policy changed")
		}
		if err := validateNodeRuntime(s.cfg, []string{"vless"}, node, current); err != nil {
			t.Fatalf("runtime validated an ignored panel policy: %v", err)
		}
		after, _ := json.Marshal(node)
		if !bytes.Equal(original, after) {
			t.Fatal("certificate policy selection mutated the panel snapshot")
		}
	}
	if s.cfg.Kernel.LogLevel != "debug" {
		t.Fatal("local certificate ownership unexpectedly blocked other panel overrides")
	}
}

func TestCertificatePolicy_DefaultRetainsPanelPriority(t *testing.T) {
	for _, source := range []string{"", "panel"} {
		s := newTestService(&fakeKernel{})
		s.cfg.Cert = config.CertConfig{Source: source, CertMode: "file", CertFile: "unused-local.pem", KeyFile: "unused-local.key", CertDir: t.TempDir()}
		s.cert = cert.NewManager(config.CertConfig{})
		s.applyRemoteOverrides(context.Background(), &model.NodeSpec{CertConfig: &config.CertConfig{CertMode: "none", Domain: "panel.example.com"}})
		if s.cfg.Cert.CertMode != "none" || s.cfg.Cert.Domain != "panel.example.com" {
			t.Fatal("default panel priority changed")
		}
	}
}

func TestCertificatePolicy_LocalDoesNotBorrowIgnoredPanelMaterial(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox"}, Cert: config.CertConfig{Source: "local", CertMode: "none"}}
	node := &model.NodeSpec{Protocol: "anytls", CertConfig: &config.CertConfig{CertMode: "self", Domain: "panel.example.com"}}
	if err := validateNodeRuntime(cfg, []string{"anytls"}, node, kernel.TLSCert{}); err == nil {
		t.Fatal("an ignored panel policy incorrectly satisfied local TLS requirements")
	}
}
