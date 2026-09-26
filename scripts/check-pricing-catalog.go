//go:build ignore

// Validate a reviewed price catalog before publishing the JSON independently.
// Usage: go run scripts/check-pricing-catalog.go /path/to/catalog.json
package main

import (
	"agentbox/internal/pricecatalog"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run scripts/check-pricing-catalog.go <catalog.json>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	catalog, err := pricecatalog.Parse(raw, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("Validated %s: %d models, sha256 %s\n", catalog.Version, len(catalog.Entries), catalog.Revision())
}
