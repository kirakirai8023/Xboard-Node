// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"path/filepath"
	"testing"

	"github.com/kirakirai8023/Xboard-Node/internal/config"
)

func TestWriteRootConfig_PreservesForwarding(t *testing.T) {
	t.Setenv("XBOARD_FORWARDING_TEST_TOKEN", "test-token")
	path := filepath.Join(t.TempDir(), "config.yml")
	root := &config.RootConfig{
		Config: config.Config{Kernel: config.KernelConfig{XrayForwarding: "compatible", Sniffing: "route_only"}},
		Instances: []config.Config{{
			Panel:  config.PanelConfig{URL: "https://panel.example.com", TokenEnv: "XBOARD_FORWARDING_TEST_TOKEN", NodeID: 1},
			Kernel: config.KernelConfig{Type: "xray", ConfigDir: t.TempDir(), XrayForwarding: "auto", Sniffing: "override_destination"},
		}},
	}
	if err := writeRootConfig(path, root); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Kernel.XrayForwarding != "compatible" || loaded.Kernel.Sniffing != "route_only" {
		t.Fatal("shared forwarding options were lost")
	}
	if len(loaded.Instances) != 1 || loaded.Instances[0].Kernel.XrayForwarding != "auto" || loaded.Instances[0].Kernel.Sniffing != "override_destination" {
		t.Fatal("instance forwarding options were lost")
	}
}
