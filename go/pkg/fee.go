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
	// RoundBank is half-to-even, matching the NUMERIC(28,6) columns the result
	// is stored in.
	return amount.RoundBank(6), nil
}
