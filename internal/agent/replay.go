package agent

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/TamasGorgics/recon-agent/internal/agent/tools"
)

// DeterministicRunID derives the replay/recording run id for one case. The id
// is a valid RFC 4122 UUID rendered from a SHA-256 digest, so the request
// transcript (including the run-id-derived untrusted wrapper token) is stable
// across processes. Production worker runs keep using a random UUIDv4.
func DeterministicRunID(caseID string) (string, error) {
	caseID = strings.ToLower(strings.TrimSpace(caseID))
	if !tools.IsUUID(caseID) {
		return "", fmt.Errorf("agent: case id %q is not a UUID", caseID)
	}
	sum := sha256.Sum256([]byte("m2-replay:" + caseID))
	var raw [16]byte
	copy(raw[:], sum[:16])
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
