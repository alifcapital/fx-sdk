package pkg

import "github.com/quagmt/udecimal"

// hundred is the percentage divisor. udecimal has no Hundred constant, and
// MustFromInt64 cannot fail for a coefficient this small.
var hundred = udecimal.MustFromInt64(100, 0)

func Percentage(amount udecimal.Decimal, percentage udecimal.Decimal) (udecimal.Decimal, error) {
	// Mul cannot overflow — udecimal falls back to big.Int — so Div by the
	// non-zero hundred is the only step that can fail.
	amount, err := amount.Mul(percentage).Div(hundred)
	if err != nil {
		return udecimal.Zero, err
	}
	// Rounded here, to the scale of the NUMERIC(28,6) columns the result is
	// stored in, so the database never has to round it itself. The mode is not
	// the database's: PostgreSQL rounds NUMERIC half away from zero, RoundBank
	// is half to even, and the two differ on an exact half. That only matters
	// for a value stored unrounded, which this is not — but anyone reusing this
	// helper on a value that goes to the database raw should use RoundHAZ.
	return amount.RoundBank(6), nil
}
