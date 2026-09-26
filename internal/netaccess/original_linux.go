//go:build linux

package netaccess

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

func platformCheck() error { return nil }
func originalDestination(c net.Conn) (string, error) {
	tcp, ok := c.(*net.TCPConn)
	if !ok {
		return "", fmt.Errorf("TCP connection required")
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return "", err
	}
	var target string
	var sockErr error
	err = raw.Control(func(fd uintptr) {
		addr, e := unix.GetsockoptIPv6Mreq(int(fd), unix.SOL_IP, 80)
		if e != nil {
			sockErr = e
			return
		}
		b := addr.Multiaddr
		target = net.JoinHostPort(net.IP(b[4:8]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(b[2:4]))))
	})
	if err != nil {
		return "", err
	}
	return target, sockErr
}
