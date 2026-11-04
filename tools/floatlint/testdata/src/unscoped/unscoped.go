package unscoped

import "strconv"

// Floating-point use is allowed here: the package is not listed in the
// floatlint config.
var Confidence = 0.95

var Parsed, _ = strconv.ParseFloat("1.5", 64)
