package synth

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// namespace is the fixed UUIDv5 namespace for every logical ID this package
// emits. It is intentionally not a public DNS/URL namespace: changing it
// changes every dataset, so it is part of the generator contract.
var namespace = [16]byte{
	0x8f, 0x3c, 0x1a, 0x52, 0x6d, 0x9e, 0x47, 0x0b,
	0xa1, 0x74, 0x2c, 0xd8, 0xe5, 0x09, 0x6b, 0x31,
}

// logicalID returns a UUIDv5 over (namespace, seed, parts...). The same seed
// and parts always produce the same UUID, on every machine and process. This is
// the only ID source for generated rows; database UUID defaults are never used.
func logicalID(seed int64, parts ...string) string {
	h := sha1.New() //nolint:gosec // UUIDv5 is defined over SHA-1.
	_, _ = h.Write(namespace[:])
	_, _ = h.Write([]byte("|seed="))
	_, _ = h.Write([]byte(strconv.FormatInt(seed, 10)))
	for _, part := range parts {
		_, _ = h.Write([]byte("|"))
		_, _ = h.Write([]byte(part))
	}
	sum := h.Sum(nil)
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50 // version 5
	id[8] = (id[8] & 0x3f) | 0x80 // RFC 4122 variant
	encoded := hex.EncodeToString(id[:])
	return strings.Join([]string{
		encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32],
	}, "-")
}

// hashHex is the lowercase SHA-256 hex digest of text.
func hashHex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// datasetVersion implements spec D-7:
//
//	sha256(generator_version|seed|config_hash)
func datasetVersion(seed int64, configHash string) string {
	return hashHex(fmt.Sprintf("%s|%d|%s", GeneratorVersion, seed, configHash))
}
