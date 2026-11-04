package bad

import (
	"math"
	"strconv"
)

var Float32Value float32 // want `float32 is not allowed.*FIN-01`

var Float64Value float64 // want `float64 is not allowed.*FIN-01`

var FloatLiteral = 1.5 // want `floating-point literal is not allowed.*FIN-01`

var HexFloatLiteral = 0x1p-2 // want `floating-point literal is not allowed.*FIN-01`

var Parsed, _ = strconv.ParseFloat("1.5", 64) // want `strconv.ParseFloat is not allowed.*FIN-01`

var Rounded = math.Round(2) // want `math.Round is not allowed.*FIN-01`

var Floored = math.Floor(2) // want `math.Floor is not allowed.*FIN-01`

var Converted = float64(1) // want `float64 is not allowed.*FIN-01`

// Allowed proves a directive only suppresses its own line.
var Allowed = 2.5    //floatlint:allow test tolerance, not money
var NotAllowed = 3.5 // want `floating-point literal is not allowed.*FIN-01`
