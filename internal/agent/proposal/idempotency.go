package proposal

// IdempotencyKey recomputes the proposal idempotency key from the canonical
// fields. It calls the same pure derivation the recorder uses and needs no
// database handle.
func IdempotencyKey(caseID, runID string, f Fields) (string, error) {
	return idempotencyKey(caseID, runID, f.Type, f.Payload, f.Rationale,
		f.Evidence, f.Confidence, f.Pattern())
}
