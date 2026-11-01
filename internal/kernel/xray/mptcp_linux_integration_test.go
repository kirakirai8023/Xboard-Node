// SPDX-License-Identifier: AGPL-3.0-only

//go:build integration && linux

package xray

import (
	"context"
	"net"
	"testing"

	"github.com/xtls/xray-core/transport/internet"
	"golang.org/x/sys/unix"
)

// Check the listening socket itself: a plain TCP client can connect to an
// MPTCP listener without negotiating MPTCP, hiding an unsafe listener default.
func TestXrayListener_MPTCPDisabledByDefault(t *testing.T) {
	t.Setenv("GODEBUG", "multipathtcp=3")
	for _, tc := range []struct {
		name    string
		sockopt *internet.SocketConfig
	}{
		{"no_socket_options", nil},
		{"empty_socket_options", &internet.SocketConfig{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener := new(internet.DefaultListener)
			ln, err := listener.Listen(context.Background(), &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}, tc.sockopt)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			raw, err := ln.(*net.TCPListener).SyscallConn()
			if err != nil {
				t.Fatal(err)
			}
			var protocol int
			var socketErr error
			if err := raw.Control(func(fd uintptr) {
				protocol, socketErr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_PROTOCOL)
			}); err != nil {
				t.Fatal(err)
			}
			if socketErr != nil {
				t.Fatal(socketErr)
			}
			if protocol != unix.IPPROTO_TCP {
				t.Fatalf("listener protocol = %d, want TCP (%d)", protocol, unix.IPPROTO_TCP)
			}
		})
	}
}
