//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// AF_VSOCK constants from linux/vm_sockets.h.
const (
	afVsock      = 40
	vsockCIDHost = 2
	vsockCIDAny  = 0xFFFFFFFF
)

type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

type vsockAddr struct {
	cid  uint32
	port uint32
}

func (a vsockAddr) Network() string { return "vsock" }
func (a vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }

// vsockConn wraps an *os.File as a net.Conn. Go's net.FileConn rejects
// AF_VSOCK because the net package only recognizes AF_INET, AF_INET6,
// and AF_UNIX (golang/go#69769).
type vsockConn struct {
	file       *os.File
	localAddr  vsockAddr
	remoteAddr vsockAddr
}

func (c *vsockConn) Read(b []byte) (int, error)         { return c.file.Read(b) }
func (c *vsockConn) Write(b []byte) (int, error)        { return c.file.Write(b) }
func (c *vsockConn) Close() error                       { return c.file.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return c.localAddr }
func (c *vsockConn) RemoteAddr() net.Addr               { return c.remoteAddr }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.file.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.file.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.file.SetWriteDeadline(t) }

type vsockListener struct {
	fd   int
	port uint32
}

func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}

	addr := sockaddrVM{
		Family: afVsock,
		Port:   port,
		CID:    vsockCIDAny,
	}

	_, _, errno := syscall.RawSyscall(
		syscall.SYS_BIND,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("bind(AF_VSOCK, port %d): %w", port, errno)
	}

	if err := syscall.Listen(fd, 8); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("listen(AF_VSOCK, port %d): %w", port, err)
	}

	return &vsockListener{fd: fd, port: port}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	nfd, _, err := syscall.Accept(l.fd)
	if err != nil {
		return nil, fmt.Errorf("accept(AF_VSOCK, port %d): %w", l.port, err)
	}
	_ = syscall.SetNonblock(nfd, true)
	f := os.NewFile(uintptr(nfd), fmt.Sprintf("vsock-accept:%d", l.port))
	return &vsockConn{
		file:      f,
		localAddr: vsockAddr{cid: vsockCIDAny, port: l.port},
	}, nil
}

func (l *vsockListener) Close() error {
	return syscall.Close(l.fd)
}

func dialVsock(cid, port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}

	addr := sockaddrVM{
		Family: afVsock,
		Port:   port,
		CID:    cid,
	}

	_, _, errno := syscall.RawSyscall(
		syscall.SYS_CONNECT,
		uintptr(fd),
		uintptr(unsafe.Pointer(&addr)),
		unsafe.Sizeof(addr),
	)
	if errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("connect(AF_VSOCK, cid=%d port=%d): %w", cid, port, errno)
	}

	_ = syscall.SetNonblock(fd, true)
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d:%d", cid, port))
	conn := &vsockConn{
		file:       f,
		localAddr:  vsockAddr{cid: 3, port: 0},
		remoteAddr: vsockAddr{cid: cid, port: port},
	}
	return conn, nil
}
