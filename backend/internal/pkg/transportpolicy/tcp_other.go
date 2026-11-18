//go:build !linux

package transportpolicy

import "syscall"

// Other platforms retain TCP keepalive; TCP_USER_TIMEOUT is Linux-specific.
var dialControl func(network, address string, c syscall.RawConn) error
