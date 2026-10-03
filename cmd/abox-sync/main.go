// abox-sync is the portable sync sidecar. It offers local preflight inspection,
// lifecycle commands and explicit sync batches gated by server capability.
package main

import (
	"fmt"
	"os"

	"agentbox/internal/syncclient"
)

func main() {
	if len(os.Args) != 2 || os.Args[1] != "--desktop" {
		fmt.Fprintln(os.Stderr, "usage: abox-sync --desktop (private stdio protocol v1; sync capability required)")
		os.Exit(2)
	}
	if err := syncclient.RunDesktop(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "abox-sync:", err)
		os.Exit(1)
	}
}
