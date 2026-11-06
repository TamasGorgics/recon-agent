package proposal

import (
	"crypto/rand"
	"fmt"
	"io"
)

// NewUUID returns a random RFC 4122 version 4 UUID from crypto/rand. It is the
// default Deps.NewID; the proposal row id is generated client-side because the
// agent role cannot use RETURNING.
func NewUUID() (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("proposal: generate uuid: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
