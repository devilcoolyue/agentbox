package syncproto

import (
	"strings"
	"testing"
	"time"
)

func TestRecoveryMaintenanceExtensionPreservesLegacyStatusValidation(t *testing.T) {
	id := strings.Repeat("a", 32)
	legacy := OperationStatus{Operation: MutationResult{ID: id, Status: "applied", Recovery: true}, Path: "file", Kind: "delete", Before: &Entry{Kind: "file", Hash: HashBytes([]byte("old")), Size: 3}}
	if err := legacy.Validate(id); err != nil {
		t.Fatal(err)
	}
	current := legacy
	current.Digest, current.Device, current.Project, current.RetirementConfirmation = strings.Repeat("b", 64), "device", "project", strings.Repeat("c", 64)
	if err := current.Validate(id); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"retiring", "retired"} {
		copy := current
		copy.Retirement = state
		if copy.Validate(id) == nil {
			t.Fatal("retired response still advertised recovery")
		}
		copy.Operation.Recovery = false
		if err := copy.Validate(id); err != nil {
			t.Fatal(err)
		}
		copy.Operation.Status = "uncertain"
		if copy.Validate(id) == nil {
			t.Fatal("uncertain retirement accepted")
		}
	}
	for _, kind := range []string{"digest", "device", "project", "confirmation", "state"} {
		copy := current
		switch kind {
		case "digest":
			copy.Digest = ""
		case "device":
			copy.Device = ""
		case "project":
			copy.Project = ""
		case "confirmation":
			copy.RetirementConfirmation = ""
		case "state":
			copy.Retirement = "deleted"
		}
		if copy.Validate(id) == nil {
			t.Fatal("partial maintenance extension accepted", kind)
		}
	}
}

func TestRecoveryStorageRefusesImpossibleCapacity(t *testing.T) {
	valid := RecoveryStorage{ActiveOperations: 1, OperationLimit: 1000, RetainedReceipts: 2, ReceiptLimit: 100000, RecoveryBytes: 3, RecoveryByteLimit: 256 << 20, MetadataBytes: 1000}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"negative_active", "negative_receipts", "negative_bytes", "negative_metadata", "over_active", "over_receipts", "over_bytes", "no_limit"} {
		copy := valid
		switch kind {
		case "negative_active":
			copy.ActiveOperations = -1
		case "negative_receipts":
			copy.RetainedReceipts = -1
		case "negative_bytes":
			copy.RecoveryBytes = -1
		case "negative_metadata":
			copy.MetadataBytes = -1
		case "over_active":
			copy.ActiveOperations = copy.OperationLimit + 1
		case "over_receipts":
			copy.RetainedReceipts = copy.ReceiptLimit + 1
		case "over_bytes":
			copy.RecoveryBytes = copy.RecoveryByteLimit + 1
		case "no_limit":
			copy.OperationLimit = 0
		}
		if copy.Validate() == nil {
			t.Fatal("impossible capacity accepted", kind)
		}
	}
}

func TestRecoveryWorkspaceLeaseGuardIncludesOtherProjectsAndExpires(t *testing.T) {
	leases := NewLeases()
	now := time.Unix(1000, 0)
	leases.now = func() time.Time { return now }
	if leases.WorkspaceActive("workspace") {
		t.Fatal("empty workspace appears leased")
	}
	if _, err := leases.Acquire("workspace", "other-project", "sibling", "other-device"); err != nil {
		t.Fatal(err)
	}
	if !leases.WorkspaceActive("workspace") || leases.WorkspaceActive("unrelated") {
		t.Fatal("workspace guard scope wrong")
	}
	now = now.Add(LeaseTTL)
	if leases.WorkspaceActive("workspace") {
		t.Fatal("expired lease blocks recovery forever")
	}
}
