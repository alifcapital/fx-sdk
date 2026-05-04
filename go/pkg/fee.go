package pkg

import "github.com/govalues/decimal"

func Percentage(amount decimal.Decimal, percentage decimal.Decimal) (decimal.Decimal, error) {
	amount, err := amount.Mul(percentage)
	if err != nil {
		return decimal.Zero, err
	}
	amount, err = amount.Quo(decimal.Hundred)
	if err != nil {
		return decimal.Zero, err
	}
	return amount, nil
}
