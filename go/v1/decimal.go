package v1

import (
	"errors"
	"fmt"

	"github.com/quagmt/udecimal"
)

// maxDecimalPlaces is the scale every monetary value carries: NUMERIC(28,6) in
// the local client_* tables and the same six digits in the Core.
const maxDecimalPlaces = 6

// ErrDecimalPrecision is returned when a monetary value carries more decimal
// places than the six that can be stored.
var ErrDecimalPrecision = errors.New("fx-sdk: more than 6 decimal places")

// checkDecimal refuses a value that the local columns — and the Core — would
// have to round.
//
// Truncation rather than a rounding mode, because the question is exactly
// whether a significant digit sits past the sixth decimal. udecimal's Prec()
// cannot answer it: it reports the scale it parsed, so "1000.1000000" claims
// seven while holding a value the column stores exactly, and a caller who pads
// with zeros has asked for nothing unusual.
func checkDecimal(field, value string) error {
	d, err := udecimal.Parse(value)
	if err != nil {
		return fmt.Errorf("fx-sdk: %s: %w", field, err)
	}
	if d.Trunc(maxDecimalPlaces).Cmp(d) != 0 {
		return fmt.Errorf("%w: %s = %q", ErrDecimalPrecision, field, value)
	}
	return nil
}
