package sandbox

import (
	"crypto/sha1" //nolint:gosec // UUIDv5 is defined over SHA-1.
	"encoding/hex"
	"strconv"
	"strings"
)

// evalScopeNamespace is the fixed UUIDv5 namespace synth/modes uses for its
// logical ids. It must stay byte-identical: eval/dataset truth signatures are
// computed over deterministic scope ids built from it.
var evalScopeNamespace = [16]byte{
	0x3b, 0x91, 0x74, 0xc2, 0x0e, 0x4d, 0x4a, 0x8f,
	0x96, 0x21, 0xb7, 0x5e, 0x18, 0x6a, 0xf0, 0x42,
}

// deterministicScopeID reproduces synth/modes.logicalID(seed, "scope", source,
// currency). The generated world's ledger.json does not carry scope ids, so
// synth.Load lets Postgres assign random UUIDs; Clone pins them back to these
// values before running the matcher so committed truth signatures are
// reproducible inside every eval clone.
func deterministicScopeID(seed int64, source, currency string) string {
	hash := sha1.New() //nolint:gosec // UUIDv5 is defined over SHA-1.
	_, _ = hash.Write(evalScopeNamespace[:])
	_, _ = hash.Write([]byte("|seed="))
	_, _ = hash.Write([]byte(strconv.FormatInt(seed, 10)))
	for _, part := range []string{"scope", source, currency} {
		_, _ = hash.Write([]byte("|"))
		_, _ = hash.Write([]byte(part))
	}
	sum := hash.Sum(nil)
	var id [16]byte
	copy(id[:], sum[:16])
	id[6] = (id[6] & 0x0f) | 0x50
	id[8] = (id[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(id[:])
	return strings.Join([]string{
		encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32],
	}, "-")
}
