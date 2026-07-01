package v1

import (
	"context"
	"fmt"

	"github.com/alifcapital/fx-sdk/go/forexv1"
)

// GetCurrencyPairs returns the currency pairs available for trading and their
// specifications. The partner is identified by the partner-id metadata attached
// to every request, so no parameters are required.
func (c *Client) GetCurrencyPairs(ctx context.Context) ([]CurrencyPair, error) {
	resp, err := c.partner.GetCurrencyPairs(ctx, &forexv1.GetCurrencyPairsRequest{})
	if err != nil {
		return nil, fmt.Errorf("fx-sdk: get currency pairs: %w", err)
	}

	pairs := make([]CurrencyPair, 0, len(resp.GetCurrencyPairs()))
	for _, p := range resp.GetCurrencyPairs() {
		pairs = append(pairs, CurrencyPair{
			Pair:             p.GetPair(),
			MinLot:           p.GetMinLot(),
			MinTradeQuantity: p.GetMinTradeQuantity(),
			ValidRatePercent: p.GetValidRatePercent(),
			NbtAvgRate:       p.GetNbtAvgRate(),
			IsActive:         p.GetIsActive(),
		})
	}
	return pairs, nil
}
