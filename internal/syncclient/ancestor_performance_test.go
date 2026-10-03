package syncclient

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"agentbox/internal/syncfs"
)

// Measures one complete Register call, including volume approval, bounded
// capability probes, ancestor overlap checks and the durable SQLite insert.
// Fixture creation and the picker-equivalent identity query are outside timing.
func BenchmarkRegisterMappedDirectory(b *testing.B) {
	private, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	state, err := OpenStateContext(b.Context(), filepath.Join(private, "state"))
	if err != nil {
		b.Fatal(err)
	}
	defer state.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		directory := filepath.Join(private, fmt.Sprintf("project-%d", i))
		if err := os.Mkdir(directory, 0700); err != nil {
			b.Fatal(err)
		}
		root, err := syncfs.OpenContext(b.Context(), directory)
		if err != nil {
			b.Fatal(err)
		}
		identity, err := root.Identity()
		root.Close()
		if err != nil {
			b.Fatal(err)
		}
		binding := planBinding()
		binding.LocalID = identity
		binding.Workspace = fmt.Sprintf("workspace-%d", i)
		b.StartTimer()
		if _, err := state.RegisterContext(b.Context(), binding, directory); err != nil {
			b.Fatal(err)
		}
	}
}
