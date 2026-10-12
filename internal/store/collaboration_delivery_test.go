package store

import (
	"strings"
	"testing"
	"time"
)

func TestCollaborationDeliveryRequiresMatchingSuccessfulVerification(t *testing.T) {
	for _, scenario := range []string{"none", "pass", "fail", "restart", "different-snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			s, u, r := preparedCollaboration(t, false)
			if _, err := s.db.Exec("UPDATE collaborations SET plan_version=1 WHERE id=?", r.ID); err != nil {
				t.Fatal(err)
			}
			// This fixture pins a reviewed snapshot. Runtime stop and hash checking
			// have separate tests; here the database delivery boundary is exercised.
			snapshot := strings.Repeat("e", 32)
			if _, err := s.db.Exec("UPDATE collaboration_steps SET state='completed' WHERE run_id=?", r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE collaborations SET state='paused',wait_reason='verification_required',snapshot_id=? WHERE id=?", snapshot, r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("INSERT INTO collaboration_artifacts(id,run_id,step_id,kind,storage_key,sha256,size_bytes,state,created_at) VALUES(?,?,'step','snapshot',?,'hash',1,'ready','fixture')", snapshot, r.ID, "snapshots/"+snapshot); err != nil {
				t.Fatal(err)
			}
			if scenario != "none" {
				if _, err := s.db.Exec(`UPDATE collaboration_configs SET config_json=json_set(config_json,'$.verification_command','test -f source') WHERE run_id=?`, r.ID); err != nil {
					t.Fatal(err)
				}
			}
			r, err := s.Collaboration(u, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.DeliverCollaboration(u, r.ID, r.Revision, snapshot); err == nil {
				t.Fatal("delivered before verification record")
			}
			v, fresh, err := s.BeginCollaborationVerification(u, r.ID, r.Revision)
			if err != nil || !fresh {
				t.Fatal(v, fresh, err)
			}
			_, fresh, err = s.BeginCollaborationVerification(u, r.ID, r.Revision)
			if err != nil && scenario == "none" {
				t.Fatal(err)
			}
			if fresh {
				t.Fatal("verification replay grants execution")
			}
			switch scenario {
			case "pass", "different-snapshot":
				err = s.FinishCollaborationVerification(u, v, "completed", "passed", time.Second)
			case "fail":
				err = s.FinishCollaborationVerification(u, v, "failed", "failed", time.Second)
			case "restart":
				err = s.RecoverCollaborations()
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "restart" {
				if _, err = s.DeliverCollaboration(u, r.ID, r.Revision, snapshot); err == nil {
					t.Fatal("verification bypassed model review")
				}
				r, err = s.QueueCollaborationReview(u, r.ID, r.Revision)
				if err != nil {
					t.Fatal(err)
				}
				work, e := s.CollaborationWork(u, r.ID)
				if e != nil || !strings.Contains(work.Input.Text, "platform_verification") || !strings.Contains(work.Input.Text, snapshot) {
					t.Fatal("missing verification evidence", e)
				}
				r = finishCollaborationFixture(t, s, u, r, "review", map[string]any{"decision": "approve", "snapshot_id": snapshot, "plan_version": r.PlanVersion, "summary": "Approved", "changes": []string{}})
			}
			id := snapshot
			if scenario == "different-snapshot" {
				id = strings.Repeat("f", 32)
			}
			result, err := s.DeliverCollaboration(u, r.ID, r.Revision, id)
			if scenario == "none" || scenario == "pass" {
				if err != nil || result.State != CollabCompleted {
					t.Fatal(result, err)
				}
				if scenario == "none" && result.WaitReason != "model_review_only" {
					t.Fatal("unrequested tests described as passing")
				}
			} else if err == nil {
				t.Fatal("failed/unknown/wrong-version result delivered")
			}
			if scenario == "restart" {
				pending, err := s.UncertainCollaborationVerifications(u, r.ID)
				if err != nil || len(pending) != 1 || pending[0].HelperID != v.HelperID {
					t.Fatal("lost persisted helper identity", pending, err)
				}
				if err = s.FinishCollaborationVerification(u, v, "completed", "late success", time.Second); err == nil {
					t.Fatal("late callback escaped recovery fence")
				}
				wrong := v
				wrong.HelperID = NewCollaborationID()
				if err = s.ConfirmCollaborationVerificationStopped(u, wrong); err == nil {
					t.Fatal("wrong helper acknowledged")
				}
				if err = s.ConfirmCollaborationVerificationStopped(u, v); err != nil {
					t.Fatal(err)
				}
				if _, err = s.DeliverCollaboration(u, r.ID, r.Revision, snapshot); err == nil {
					t.Fatal("stop proof treated as test success")
				}
				_, fresh, err = s.BeginCollaborationVerification(u, r.ID, r.Revision)
				if err != nil || fresh {
					t.Fatal("recovered test reran", fresh, err)
				}
				cfg, err := s.CollaborationConfig(r.ID, r.ConfigVersion)
				if err != nil {
					t.Fatal(err)
				}
				next := applyCollaborationFixture(t, s, u, r, "configure", cfg)
				if next.ConfigVersion != r.ConfigVersion+1 || next.State != CollabAccepted {
					t.Fatal("same configuration did not create explicit new attempt", next)
				}
			}
		})
	}
}
