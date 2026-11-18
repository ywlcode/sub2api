//go:build linux

package proxyutil

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Read the options on a real connection made through ConfigureTransportProxy.
// This catches the original bug even when the ordinary upstream dialer is right:
// SOCKS replaces that dialer, so configuration-only assertions are insufficient.
func TestSOCKSConnectionCarriesTCPFailureProtection(t *testing.T) {
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer func() { _ = listener.Close() }()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				header := make([]byte, 2)
				if _, err = io.ReadFull(conn, header); err != nil {
					done <- err
					return
				}
				if _, err = io.CopyN(io.Discard, conn, int64(header[1])); err != nil {
					done <- err
					return
				}
				if _, err = conn.Write([]byte{5, 0}); err != nil {
					done <- err
					return
				}
				request := make([]byte, 4)
				if _, err = io.ReadFull(conn, request); err != nil {
					done <- err
					return
				}
				n := 0
				switch request[3] {
				case 1:
					n = 4
				case 4:
					n = 16
				case 3:
					if _, err = io.ReadFull(conn, header[:1]); err != nil {
						done <- err
						return
					}
					n = int(header[0])
				}
				if _, err = io.CopyN(io.Discard, conn, int64(n+2)); err != nil {
					done <- err
					return
				}
				if _, err = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80}); err != nil {
					done <- err
					return
				}
				_, err = io.Copy(io.Discard, conn)
				done <- err
			}()
			proxyURL, err := url.Parse(scheme + "://" + listener.Addr().String())
			require.NoError(t, err)
			transport := &http.Transport{}
			require.NoError(t, ConfigureTransportProxy(transport, proxyURL))
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			conn, err := transport.DialContext(ctx, "tcp", "127.0.0.1:443")
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			// x/net returns its internal socks.Conn embedding the real net.Conn.
			// Read that exported field without importing an internal package.
			wrapped := reflect.Indirect(reflect.ValueOf(conn)).FieldByName("Conn")
			require.True(t, wrapped.IsValid(), "%T", conn)
			tcp, ok := wrapped.Interface().(*net.TCPConn)
			require.True(t, ok, "%T", conn)
			raw, err := tcp.SyscallConn()
			require.NoError(t, err)
			for _, opt := range []struct {
				name             string
				level, key, want int
			}{
				{"SO_KEEPALIVE", syscall.SOL_SOCKET, syscall.SO_KEEPALIVE, 1},
				{"TCP_KEEPIDLE", syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE, 15},
				{"TCP_KEEPINTVL", syscall.IPPROTO_TCP, syscall.TCP_KEEPINTVL, 5},
				{"TCP_KEEPCNT", syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, 3},
				{"TCP_USER_TIMEOUT", syscall.IPPROTO_TCP, 0x12, 30000},
			} {
				var value int
				var sockErr error
				require.NoError(t, raw.Control(func(fd uintptr) { value, sockErr = syscall.GetsockoptInt(int(fd), opt.level, opt.key) }))
				require.NoError(t, sockErr)
				if value != opt.want {
					t.Errorf("%s: got %d, want %d", opt.name, value, opt.want)
				}
			}
			require.NoError(t, conn.Close())
			require.NoError(t, <-done)
		})
	}
}
