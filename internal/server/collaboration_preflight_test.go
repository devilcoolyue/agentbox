package server

import (
	"agentbox/internal/dockerx"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCollaborationSourcePreflightFailures(t *testing.T) {
	for _, tc := range []struct {
		name, stage, output string
		cause, want         error
		code                string
	}{
		{name: "not-repository", stage: "status", cause: &dockerx.CommandError{Code: 128, Stderr: "fatal: not a git repository (or any of the parent directories): .git"}, want: errCollaborationSourceRepository, code: "collaboration_source_not_repository"},
		{name: "missing-directory", stage: "status", cause: &dockerx.CommandError{Code: 128, Stderr: "fatal: cannot change to '/workspace/private': No such file or directory"}, want: errCollaborationSourceRepository, code: "collaboration_source_not_repository"},
		{name: "dirty", stage: "status", output: " M private.txt\n", want: errCollaborationSourceDirty, code: "collaboration_source_dirty"},
		{name: "no-commit", stage: "rev-parse", cause: &dockerx.CommandError{Code: 128, Stderr: "fatal: Needed a single revision"}, want: errCollaborationSourceCommit, code: "collaboration_source_no_commit"},
		{name: "permission-is-not-missing-repo", stage: "status", cause: &dockerx.CommandError{Code: 128, Stderr: "fatal: cannot change to '/workspace/private': Permission denied"}, code: "internal_error"},
		{name: "deadline", stage: "status", cause: context.DeadlineExceeded, code: "operation_timed_out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkCollaborationSource(func(args ...string) (string, error) {
				if args[0] == tc.stage {
					return tc.output, tc.cause
				}
				if args[0] == "status" {
					return "", nil
				}
				t.Fatal("continued after failed source check")
				return "", nil
			})
			want := tc.want
			if want == nil {
				want = tc.cause
			}
			if !errors.Is(err, want) {
				t.Fatalf("error=%v want=%v", err, want)
			}
			w := httptest.NewRecorder()
			writeCollaborationError(w, httptest.NewRequest("POST", "/api/collaboration/preflight", nil), err)
			assertProblem(t, w.Body.Bytes(), tc.code)
			if strings.Contains(w.Body.String(), "private") {
				t.Fatal("source detail leaked")
			}
		})
	}
}
func TestCollaborationSourcePreflightCleanCommit(t *testing.T) {
	head := strings.Repeat("a", 40)
	calls := []string{}
	baseline, err := checkCollaborationSource(func(args ...string) (string, error) {
		calls = append(calls, args[0])
		if args[0] == "ls-tree" {
			return "100644 blob e69de29bb2d1d6434b8b29ae775ad8c2e48c5391       0\tREADME.md\x00", nil
		}
		if args[0] == "rev-parse" {
			return head + "\n", nil
		}
		return "", nil
	})
	if err != nil || baseline != head || strings.Join(calls, ",") != "status,rev-parse,ls-tree" {
		t.Fatalf("baseline=%q calls=%v err=%v", baseline, calls, err)
	}
}
