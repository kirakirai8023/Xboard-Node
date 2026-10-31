// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveManagementShortcut_Ownership(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		removed bool
	}{
		{"managed", "#!/usr/bin/env bash\n# Xboard-Node management shortcut\n", true},
		{"unrelated", "#!/bin/sh\necho another program\n", false},
		{"similar marker", "# Xboard-Node management shortcut for another app\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "xb")
			if err := os.WriteFile(path, []byte(tc.content), 0755); err != nil {
				t.Fatal(err)
			}
			if err := removeManagementShortcut(path); err != nil {
				t.Fatal(err)
			}
			_, err := os.Stat(path)
			if os.IsNotExist(err) != tc.removed {
				t.Fatalf("removed=%v, want %v", os.IsNotExist(err), tc.removed)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "missing")
	if err := removeManagementShortcut(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := removeManagementShortcut(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed an unrelated directory")
	}
}
