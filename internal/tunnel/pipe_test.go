package tunnel

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
)

func TestPipePreservesHalfCloseThroughYamux(t *testing.T) {
	// The target does not reply until it sees EOF. Closing it in both directions
	// when request input finishes would silently truncate the response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	targetDone := make(chan struct{})
	go func() {
		defer close(targetDone)
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		data, _ := io.ReadAll(c)
		c.Write(append([]byte("reply:"), data...))
	}()
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
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		stream, e := client.AcceptStream()
		if e != nil {
			return
		}
		target, e := net.Dial("tcp", ln.Addr().String())
		if e != nil {
			stream.Close()
			return
		}
		Pipe(stream, target)
	}()
	stream, err := server.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = stream.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}
	stream.Close()
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "reply:payload" {
		t.Fatalf("response truncated: %q", got)
	}
	<-targetDone
	<-done
}
