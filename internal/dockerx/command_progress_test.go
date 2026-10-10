package dockerx

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestProgressWriterSeparatesRedrawsFromErrors(t *testing.T) {
	var lines []string
	var errOut bytes.Buffer
	w := &progressWriter{next: &commandWriter{buf: &errOut, limit: 8 << 10}, fn: func(s string) { lines = append(lines, s) }}
	// Docker delivers stderr in arbitrary chunks; split mid-segment on purpose.
	for _, chunk := range []string{"Receiving objects:  1% (1/100)\rReceiv", "ing objects:  50% (50/100)\r", "Receiving objects: 100% (100/100), done.\nfatal: remote hung up", ""} {
		_, _ = w.Write([]byte(chunk))
	}
	w.flush()
	want := []string{"Receiving objects:  1% (1/100)", "Receiving objects:  50% (50/100)", "Receiving objects: 100% (100/100), done.", "fatal: remote hung up"}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %q", lines)
	}
	// Redraws stay out of the bounded error output; final lines and an
	// unterminated failure message are kept exactly as before.
	if got := errOut.String(); got != "Receiving objects: 100% (100/100), done.\nfatal: remote hung up" {
		t.Fatalf("error output = %q", got)
	}
}

func TestProgressWriterBoundsLongSegments(t *testing.T) {
	var lines []string
	var errOut bytes.Buffer
	w := &progressWriter{next: &commandWriter{buf: &errOut, limit: 8 << 10}, fn: func(s string) { lines = append(lines, s) }}
	_, _ = w.Write([]byte(strings.Repeat("x", 20<<10) + "\n"))
	if len(lines) != 1 || len(lines[0]) != progressCallbackLimit {
		t.Fatalf("callback got %d lines, first %d bytes", len(lines), len(lines[0]))
	}
	if errOut.Len() != 8<<10 {
		t.Fatalf("error output kept %d bytes", errOut.Len())
	}
}

func TestStderrLinesContext(t *testing.T) {
	if stderrLines(context.Background()) != nil {
		t.Fatal("plain context has a progress callback")
	}
	called := false
	ctx := WithStderrLines(context.Background(), func(string) { called = true })
	stderrLines(ctx)("x")
	if !called {
		t.Fatal("callback not carried by context")
	}
}
