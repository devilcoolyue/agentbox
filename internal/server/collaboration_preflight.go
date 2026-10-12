package server

import (
	"errors"
	"strings"

	"agentbox/internal/collaboration"
	"agentbox/internal/dockerx"
)

var (
	errCollaborationSourceRepository = errors.New("collaboration source is not a Git repository")
	errCollaborationSourceCommit     = errors.New("collaboration source has no commit")
	errCollaborationSourceDirty      = errors.New("collaboration source has uncommitted changes")
)

// Keep expected source problems separate from transport failures. Never expose
// repository-controlled Git output, paths or configuration through API errors.
func checkCollaborationSource(run func(...string) (string, error)) (string, error) {
	status, err := run("status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		var command *dockerx.CommandError
		if errors.As(err, &command) && command.Code == 128 {
			if strings.Contains(command.Stderr, "not a git repository") || strings.Contains(command.Stderr, "cannot change to") && strings.Contains(command.Stderr, "No such file or directory") {
				return "", errCollaborationSourceRepository
			}
		}
		return "", err
	}
	if status != "" {
		return "", errCollaborationSourceDirty
	}
	head, err := run("rev-parse", "--verify", "HEAD")
	if err != nil {
		var command *dockerx.CommandError
		if errors.As(err, &command) && command.Code == 128 && strings.Contains(command.Stderr, "Needed a single revision") {
			return "", errCollaborationSourceCommit
		}
		return "", err
	}
	baseline := strings.TrimSpace(head)
	listing, err := run("ls-tree", "-rlz", "--full-tree", baseline)
	if err != nil {
		return "", err
	}
	_, err = collaboration.ParseGitTree([]byte(listing), collaborationSnapshotLimits)
	return baseline, err
}
