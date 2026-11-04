package allow

import "math"

// Confidence is a model score, not a monetary amount.
var Confidence float64 //floatlint:allow confidence is a 0..1 score, not money

// Latency is a non-monetary duration in milliseconds.
var Latency = math.Round(2) //floatlint:allow latency in milliseconds, not money

// Threshold is a non-monetary test tolerance.
var Threshold = 1.5 //floatlint:allow test threshold for non-monetary behaviour
