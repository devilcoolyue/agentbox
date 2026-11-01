package tunnel

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

// TestLinkDialsSelfOnLoopback drives a real stream through Link: the server
// asks for the reserved self name and the link reaches its own loopback.
func TestLinkDialsSelfOnLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = c.Write([]byte("SSH-2.0-synthetic\r\n"))
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	left, right := net.Pipe()
	server, err := yamux.Client(left, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := yamux.Server(right, nil)
	if err != nil {
		t.Fatal(err)
	}
	wl, err := ParseWhitelist([]string{SelfHost + ":" + port})
	if err != nil {
		t.Fatal(err)
	}
	go (&Link{Whitelist: wl, DialTimeout: 2 * time.Second}).Serve(client)

	stream, err := server.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	if err := WriteConnect(stream, SelfHost+":"+port); err != nil {
		t.Fatal(err)
	}
	status := make([]byte, 1)
	if _, err := io.ReadFull(stream, status); err != nil || status[0] != StatusOK {
		t.Fatalf("status = %v, %v", status, err)
	}
	banner, _ := io.ReadAll(stream)
	if string(banner) != "SSH-2.0-synthetic\r\n" {
		t.Fatalf("banner = %q", banner)
	}
}
