package syncclient

import (
	"errors"

	"golang.org/x/sys/windows"
)

func transientNetworkErrno(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, windows.WSAECONNRESET) ||
		errors.Is(err, windows.WSAECONNABORTED) || errors.Is(err, windows.WSAETIMEDOUT) ||
		errors.Is(err, windows.WSAENETUNREACH) || errors.Is(err, windows.WSAEHOSTUNREACH)
}
