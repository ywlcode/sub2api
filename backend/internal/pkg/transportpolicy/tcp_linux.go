//go:build linux

package transportpolicy

import (
	"log/slog"
	"syscall"
)

// TCP_USER_TIMEOUT also bounds unacknowledged writes, when TCP keepalive does
// not run. It does not require application output on an otherwise healthy peer.
func dialControl(network, _ string, raw syscall.RawConn) error {
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, 0x12, int(TCPUserTimeout.Milliseconds()))
	}); err != nil {
		sockErr = err
	}
	if sockErr != nil {
		slog.Warn("transport_tcp_user_timeout_not_set", "network", network, "error", sockErr)
	}
	return nil
}
