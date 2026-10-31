// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigInit_TokenFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	credentials := filepath.Join(dir, "credentials.env")
	if err := os.WriteFile(file, []byte("fixture-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--mode", "node", "--panel-url", "https://panel.example.com", "--node-id", "1", "--kernel", "xray", "--token-file", file,
		"--output", filepath.Join(dir, "config.yml"), "--credentials-out", credentials, "--install-root", dir}
	if err := runConfigInit(args); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(credentials)
	if err != nil || !strings.Contains(string(content), "fixture-secret") {
		t.Fatalf("credentials were not generated: %v", err)
	}
	if err := runConfigInit(append(args, "--token", "conflicting-secret")); err == nil || strings.Contains(err.Error(), "conflicting-secret") {
		t.Fatal("conflicting token arguments were not rejected safely")
	}
	if err := os.WriteFile(file, []byte("first\nsecond"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runConfigInit(args); err == nil || strings.Contains(err.Error(), "second") {
		t.Fatal("multiline token was not rejected safely")
	}
	if err := runConfigInit([]string{"--token-file"}); err == nil {
		t.Fatal("missing token file argument was not rejected")
	}
}
