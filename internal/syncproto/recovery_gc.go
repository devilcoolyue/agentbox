package syncproto

// RecoveryStorage separates live operation slots from permanent replay receipts.
// Retiring operations occupy both a live slot and a reserved receipt slot until
// their old bytes have been durably removed. Metadata includes retained receipts.
type RecoveryStorage struct {
	ActiveOperations  int   `json:"active_operations"`
	OperationLimit    int   `json:"operation_limit"`
	RetainedReceipts  int   `json:"retained_receipts"`
	ReceiptLimit      int   `json:"receipt_limit"`
	RecoveryBytes     int64 `json:"recovery_bytes"`
	RecoveryByteLimit int64 `json:"recovery_byte_limit"`
	MetadataBytes     int64 `json:"metadata_bytes"`
}

func (s RecoveryStorage) Validate() error {
	if s.ActiveOperations < 0 || s.OperationLimit < 1 || s.RetainedReceipts < 0 || s.ReceiptLimit < 1 || s.RecoveryBytes < 0 || s.RecoveryByteLimit < 1 || s.MetadataBytes < 0 || s.ActiveOperations > s.OperationLimit || s.RetainedReceipts > s.ReceiptLimit || s.RecoveryBytes > s.RecoveryByteLimit {
		return ErrInvalid
	}
	return nil
}

type RetireOperationRequest struct {
	Device       string `json:"device"`
	Digest       string `json:"digest"`
	Confirmation string `json:"confirmation"`
}

func (r RetireOperationRequest) Validate() error {
	if !validIdentity(r.Device) || !ValidHash(r.Digest) || !ValidHash(r.Confirmation) {
		return ErrInvalid
	}
	return nil
}
