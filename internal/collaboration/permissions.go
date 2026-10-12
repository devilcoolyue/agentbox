package collaboration

import (
	"os"
	"path"
	"strings"

	"agentbox/internal/safefs"
)

// Project destinations are private staging trees. Apply declared directory
// modes through confined handles after creation: the service's UMask=0077 must
// still protect metadata, but cannot remove the traversal bits containers need.
// Existing paths are checked without changing their permissions.
func mkdirProjectDirs(root *safefs.Root, name string, mode os.FileMode) error {
	if name == "." {
		return nil
	}
	if !validProjectPath(name) {
		return ErrInvalid
	}
	prefix := ""
	for _, part := range strings.Split(name, "/") {
		prefix = path.Join(prefix, part)
		err := root.Mkdir(prefix, mode)
		if err != nil && !os.IsExist(err) {
			return err
		}
		if err == nil {
			if err = root.ChmodDir(prefix, mode); err != nil {
				return err
			}
		}
		dir, err := root.Sub(prefix)
		if err != nil {
			return err
		}
		if err = dir.Close(); err != nil {
			return err
		}
	}
	return nil
}
