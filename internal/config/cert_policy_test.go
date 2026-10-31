// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCertificatePolicy_ExplicitLocalSelection(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		remote       *CertConfig
		fromPanel    bool
		mode         string
	}{
		{"default_panel_none", "", &CertConfig{CertMode: "none"}, true, "none"},
		{"explicit_panel", "panel", &CertConfig{CertMode: "self"}, true, "self"},
		{"local_over_panel_none", "local", &CertConfig{CertMode: "none"}, false, "file"},
		{"local_over_panel_dns", "local", &CertConfig{CertMode: "dns"}, false, "file"},
		{"normalized_local", " LOCAL ", &CertConfig{CertMode: "none"}, false, "file"},
		{"absent_panel", "", nil, false, "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local := CertConfig{Source: tc.source, CertMode: "file", CertFile: "fixture.pem", KeyFile: "fixture.key"}
			policy, fromPanel := ResolveCertificatePolicy(local, tc.remote)
			if policy.CertMode != tc.mode || fromPanel != tc.fromPanel {
				t.Fatalf("policy=%+v panel=%t", policy, fromPanel)
			}
			if err := local.ValidateSource(); err != nil {
				t.Fatal(err)
			}
		})
	}
	if err := (CertConfig{Source: "unknown"}).ValidateSource(); err == nil {
		t.Fatal("accepted invalid certificate source")
	}
}

func TestLoadRoot_CertificateSourceInheritance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	fixture := `cert:
  source: local
  cert_mode: file
  cert_file: /fixture.pem
  key_file: /fixture.key
instances:
  - panel:
      url: https://panel.example.com
      token: fixture-token
      node_id: 1
  - panel:
      url: https://panel.example.com
      token: fixture-token
      node_id: 2
    cert:
      source: panel
`
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Instances) != 2 || !root.Instances[0].Cert.UsesLocalSource() || root.Instances[1].Cert.UsesLocalSource() {
		t.Fatal("certificate source inheritance or explicit panel override lost")
	}
	root.Instances[0].Cert.Source = "unknown"
	if err := root.validateRoot(); err == nil {
		t.Fatal("unknown certificate source passed root validation")
	}
	root.Instances[0].Cert.Source = "local"
	root.Instances[0].Nodes = []NodeEntry{{NodeID: 1, Cert: &CertConfig{Source: "unknown"}}}
	if err := root.validateRoot(); err == nil {
		t.Fatal("unknown per-node certificate source passed validation")
	}
}
