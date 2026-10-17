package modelgw

import (
	"crypto/tls"
	"net/http"
)

// DedicatedTransport returns a copy of t that opens a separate HTTP/1.1
// TCP/TLS connection per request, so cancellation closes the connection
// itself. The caller's transport is not modified.
func DedicatedTransport(t *http.Transport) *http.Transport {
	t = t.Clone()
	t.Protocols = new(http.Protocols)
	t.Protocols.SetHTTP1(true)
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	} else {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	t.DisableKeepAlives = true
	return t
}
