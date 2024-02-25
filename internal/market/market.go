package market

import (
	"context"
	"errors"
)

var SymbolNotFoundError = errors.New("symbol not found")

type Market interface {
	// GetCoins returns the current price of all coins on the market.
	GetCoins(ctx context.Context) (CoinMap, error)

	// GetSymbolInfo returns the symbol info for the given symbol.
	GetSymbolInfo(ctx context.Context, symbol string) (SymbolInfo, error)

	// Buy buys the given quantity of the given coin.
	Buy(ctx context.Context, coin string, quantity float64) (BuyOrder, error)
}
