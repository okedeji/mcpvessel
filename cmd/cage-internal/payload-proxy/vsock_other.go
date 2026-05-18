//go:build !linux

package main

import (
	"fmt"
	"net"
)

const vsockCIDHost = 2

// On non-Linux, the proxy is built but not actually invoked. These
// stubs let it compile so make test/build works on macOS.
type vsockListener struct{ port uint32 }

func listenVsock(port uint32) (*vsockListener, error) {
	return nil, fmt.Errorf("vsock not supported on this platform")
}
func (l *vsockListener) Accept() (net.Conn, error) { return nil, fmt.Errorf("vsock unsupported") }
func (l *vsockListener) Close() error              { return nil }

func dialVsock(cid, port uint32) (net.Conn, error) {
	return nil, fmt.Errorf("vsock not supported on this platform")
}
