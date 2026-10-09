package beamapi

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Credits is an amount of Beam credit.
//
// Beam bills in hundredths of a credit and a balance can be negative, so an
// amount is a decimal rather than a whole number. Older API responses carry up
// to six decimal places; the CLI reads those unchanged and displays every
// amount at two decimal places, rounded up.
type Credits float64

// Hundredths returns the amount in hundredths of a credit, rounded up (towards
// positive infinity). The amount is first taken at the six decimal places the
// API may send, so float noise such as 0.07*100 = 7.000000000000001 never
// rounds a whole amount up by another hundredth.
func (c Credits) Hundredths() int64 {
	millionths := int64(math.Round(float64(c) * 1e6))
	if millionths <= 0 {
		// Integer division truncates towards zero, which rounds a negative
		// amount up.
		return millionths / 10_000
	}
	return (millionths + 9_999) / 10_000
}

// String renders the amount rounded up to at most two decimal places, without
// trailing zeros: 0.04, 100, -2.5. A tiny negative amount rounds up to zero and
// never reads as "-0".
func (c Credits) String() string {
	hundredths := c.Hundredths()
	sign := ""
	if hundredths < 0 {
		sign = "-"
		hundredths = -hundredths
	}
	text := sign + strconv.FormatInt(hundredths/100, 10)
	if fraction := hundredths % 100; fraction != 0 {
		text += strings.TrimRight(fmt.Sprintf(".%02d", fraction), "0")
	}
	return text
}
