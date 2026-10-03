package syncclient

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDesktopInspectionThenPingOnSamePipe(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"keep.txt": "hello", "skip.log": "ignored", ".agentboxignore": "*.log\n"} {
		if err = os.WriteFile(filepath.Join(directory, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	input, commands := io.Pipe()
	output, responses := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- RunDesktop(input, responses); responses.Close() }()
	defer input.Close()
	defer output.Close()
	defer commands.Close()
	decoder := json.NewDecoder(output)
	var ready event
	if err = decoder.Decode(&ready); err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(commands)
	if err = encoder.Encode(command{Version: 1, ID: "scan", Type: "inspect", Directory: directory}); err != nil {
		t.Fatal(err)
	}
	var inspected event
	if err = decoder.Decode(&inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Type != "inspected" || inspected.Inspection == nil || inspected.Inspection.Files != 2 {
		t.Fatalf("inspection: %+v", inspected)
	}
	if err = encoder.Encode(command{Version: 1, ID: "health", Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	var pong event
	if err = decoder.Decode(&pong); err != nil || pong.Type != "pong" {
		t.Fatalf("ping after inspect: %+v %v", pong, err)
	}
	commands.Close()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("EOF did not stop inspected worker")
	}
}

func TestInspectionErrorDoesNotLeakAbsolutePath(t *testing.T) {
	_, err := InspectLocal(t.Context(), filepath.Join(t.TempDir(), "private-missing-project"))
	if err == nil {
		t.Fatal("missing root accepted")
	}
	if code := inspectionError(err); code != "inspection_io" {
		t.Fatal(code)
	}
}
