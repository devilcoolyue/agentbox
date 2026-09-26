//go:build !linux

package netaccess

import (
	"fmt"
	"net"
)

func platformCheck() error                         { return fmt.Errorf("transparent network helper requires Linux") }
func originalDestination(net.Conn) (string, error) { return "", platformCheck() }
