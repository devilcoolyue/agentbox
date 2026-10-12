package collaboration

import "strings"

// Fixed output roots are part of the verification contract. Existing entries,
// even under these roots, must still remain identical to the reviewed snapshot.
func VerificationOutputPath(path string) bool {
	for _, root := range []string{"build", "dist", "out", "target", "coverage", ".cache"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}
