package syncclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Keep raw transport errors (addresses, proxy credentials, certificate details)
// inside this process. Only a positively identified temporary failure gains this
// marker; ErrTransport alone includes permanent TLS and configuration failures.
var errTransientTransport = fmt.Errorf("%w: temporary connection failure", ErrTransport)

func transportFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return ErrTransport
	}
	var network net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		transientNetworkErrno(err) || errors.As(err, &network) && network.Timeout() {
		return errTransientTransport
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.IsTemporary && !dns.IsNotFound {
		return errTransientTransport
	}
	return ErrTransport
}

// This does not retry HTTP itself. Only a failed read-only preview may expose
// this classification to the bounded UI scheduler; failed writes still pause.
func transientPreviewFailure(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errTransientTransport) {
		return true
	}
	var rejected *HTTPError
	if errors.As(err, &rejected) {
		switch rejected.Status {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}
