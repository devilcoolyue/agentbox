package protocol

// ProblemDetails extends the legacy error string. Values must come from the
// server's allowlist; raw provider/OS errors are not part of this contract.
type ProblemDetails struct {
	Code        string `json:"code"`
	OperationID string `json:"operation_id"`
	Hint        string `json:"hint"`
	Action      string `json:"action"`
	Retryable   bool   `json:"retryable"`
}

type APIProblem struct {
	Error string `json:"error"`
	ProblemDetails
}
