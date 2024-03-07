package bot

import (
	"context"
	"fmt"
	"github.com/sleeyax/go-crypto-volatility-trading-bot/internal/config"
	"github.com/sleeyax/go-crypto-volatility-trading-bot/internal/database"
	"github.com/sleeyax/go-crypto-volatility-trading-bot/internal/database/models"
	"github.com/sleeyax/go-crypto-volatility-trading-bot/internal/market"
	"github.com/sleeyax/go-crypto-volatility-trading-bot/internal/utils"
	"go.uber.org/zap"
	"math"
	"strconv"
	"sync"
	"time"
)

const significantPriceChangeThreshold = 0.8

type Bot struct {
	market           market.Market
	db               database.Database
	volatilityWindow *VolatilityWindow
	config           *config.Configuration
	botLog           *zap.SugaredLogger
	buyLog           *zap.SugaredLogger
	sellLog          *zap.SugaredLogger
}

func New(config *config.Configuration, market market.Market, db database.Database) *Bot {
	sugaredLogger := createLogger(config.LoggingOptions).Named("bot")
	return &Bot{
		market:           market,
		db:               db,
		volatilityWindow: NewVolatilityWindow(config.TradingOptions.RecheckInterval),
		config:           config,
		botLog:           sugaredLogger,
		buyLog:           sugaredLogger.Named("buy"),
		sellLog:          sugaredLogger.Named("sell"),
	}
}

func (b *Bot) flushLogs() {
	_ = b.botLog.Sync()
	_ = b.buyLog.Sync()
	_ = b.sellLog.Sync()
}

// Start starts monitoring the market for price changes.
func (b *Bot) Start(ctx context.Context) {
	defer b.flushLogs()
	b.botLog.Info("Bot started.")

	var wg sync.WaitGroup
	wg.Add(2)

	go b.sell(ctx, &wg)
	go b.buy(ctx, &wg)

	// Wait for both buy and sell goroutines to finish.
	wg.Wait()

	b.botLog.Info("Bot stopped.")
}

func (b *Bot) buy(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	b.buyLog.Debug("Watching coins to buy.")

	if err := b.updateLatestCoins(ctx); err != nil {
		panic(fmt.Sprintf("failed to load initial latest coins: %s", err))
	}

	for {
		select {
		case <-ctx.Done():
			b.buyLog.Debug("Bot stopped buying coins.")
			return
		default:
			// Wait until the next recheck interval.
			lastRecord := b.volatilityWindow.GetLatestRecord()
			delta := utils.CalculateTimeDuration(b.config.TradingOptions.TimeDifference, b.config.TradingOptions.RecheckInterval)
			if time.Since(lastRecord.time) < delta {
				interval := delta - time.Since(lastRecord.time)
				b.buyLog.Debugf("Waiting %s.", interval.Round(time.Second))
				time.Sleep(interval)
			}

			// Fetch the latest coins again after the waiting period.
			if err := b.updateLatestCoins(ctx); err != nil {
				b.buyLog.Errorf("Failed to update latest coins: %s.", err)
				continue
			}

			// Identify volatile coins in the current time window and trade them if any are found.
			volatileCoins := b.volatilityWindow.IdentifyVolatileCoins(b.config.TradingOptions.ChangeInPrice)
			b.buyLog.Infof("Found %d volatile coins.", len(volatileCoins))
			for _, volatileCoin := range volatileCoins {
				b.buyLog.Infof("Coin %s has gained %.2f%% within the last %d minutes.", volatileCoin.Symbol, volatileCoin.Percentage, b.config.TradingOptions.TimeDifference)

				// Skip if the coin has already been bought.
				if b.db.HasOrder(models.BuyOrder, b.market.Name(), volatileCoin.Symbol) {
					b.buyLog.Warnf("Already bought %s. Skipping.", volatileCoin.Symbol)
					continue
				}

				// Skip if the max amount of buy orders has been reached.
				if maxBuyOrders := int64(b.config.TradingOptions.MaxCoins); maxBuyOrders != 0 && b.db.CountOrders(models.BuyOrder, b.market.Name()) >= maxBuyOrders {
					b.buyLog.Warnf("Max amount of buy orders reached. Skipping.")
					continue
				}

				// Skip if the coin has been sold very recently (within the cool-off period)
				if coolOffDelay := time.Duration(b.config.TradingOptions.CoolOffDelay) * time.Minute; coolOffDelay != 0 {
					lastOrder, ok := b.db.GetLastOrder(models.SellOrder, b.market.Name(), volatileCoin.Symbol)
					if ok && time.Since(lastOrder.CreatedAt) < coolOffDelay {
						b.buyLog.Warnf("Already bought %s within the configured cool-off period of %s. Skipping.", volatileCoin.Symbol, coolOffDelay)
						continue
					}
				}

				// Determine the correct volume to buy based on the configured quantity.
				volume, err := b.convertVolume(ctx, b.config.TradingOptions.Quantity, volatileCoin)
				if err != nil {
					b.buyLog.Errorf("Failed to convert volume. Skipping the trade: %s", err)
					continue
				}

				b.buyLog.Infow(fmt.Sprintf("Buying %.2f %s of %s.", volume, b.config.TradingOptions.PairWith, volatileCoin.Symbol),
					"volume", volume,
					"pair_with", b.config.TradingOptions.PairWith,
					"symbol", volatileCoin.Symbol,
					"price", volatileCoin.Price,
					"percentage", volatileCoin.Percentage,
					"testMode", b.config.EnableTestMode,
				)

				order := models.Order{
					Market:     b.market.Name(),
					Type:       models.BuyOrder,
					Volume:     volume,
					TakeProfit: &b.config.TradingOptions.TakeProfit,
					StopLoss:   &b.config.TradingOptions.StopLoss,
				}

				// Pretend to buy the coin and save the order if test mode is enabled.
				if b.config.EnableTestMode {
					order.Order = market.Order{
						OrderID:         0,
						Symbol:          volatileCoin.Symbol,
						Price:           volatileCoin.Price,
						TransactionTime: time.Now(),
					}
					order.IsTestMode = true
				} else {
					// Otherwise, buy the coin and save the real order.
					buyOrder, err := b.market.Buy(ctx, volatileCoin.Symbol, volume)
					if err != nil {
						b.buyLog.Errorf("Failed to buy %s: %s.", volatileCoin.Symbol, err)
						continue
					}

					order.Order = buyOrder
				}

				b.db.SaveOrder(order)
			}
		}
	}
}

func (b *Bot) sell(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	b.sellLog.Debug("Watching coins to sell.")

	for {
		select {
		case <-ctx.Done():
			b.sellLog.Debug("Bot stopped selling coins.")
			return
		default:
			coins, err := b.market.GetCoins(ctx)
			if err != nil {
				b.sellLog.Errorf("Failed to fetch coins: %s.", err)
				continue
			}

			orders := b.db.GetOrders(models.BuyOrder, b.market.Name())
			for _, boughtCoin := range orders {
				takeProfit := boughtCoin.Price + (boughtCoin.Price*(*boughtCoin.TakeProfit))/100
				stopLoss := boughtCoin.Price + (boughtCoin.Price*(-1*math.Abs(*boughtCoin.StopLoss)))/100
				currentPrice := coins[boughtCoin.Symbol].Price
				buyPrice := boughtCoin.Price
				priceChangePercentage := (currentPrice - buyPrice) / buyPrice * 100

				// Check that the price is above the take profit and readjust SL and TP accordingly if trialing stop loss is used.
				if b.config.TradingOptions.TrailingStopOptions.Enable && currentPrice >= takeProfit {
					trailingStopOptions := b.config.TradingOptions.TrailingStopOptions

					// Calculate trailing stop loss and take profit.
					tp := priceChangePercentage + trailingStopOptions.TrailingTakeProfit
					var sl float64
					if priceChangePercentage >= significantPriceChangeThreshold {
						// If the price has changed much we make the stop loss trail closely match the take profit.
						// This way we don't lose this increase in price if it falls back.
						sl = tp - trailingStopOptions.TrailingStopLoss
					} else {
						// If the price has changed little we make the stop loss trail loosely match the take profit.
						// This way we don't get locked out of the trade prematurely.
						sl = *boughtCoin.TakeProfit - trailingStopOptions.TrailingStopLoss
					}
					if sl <= 0 {
						// Revert to the current stop loss if the calculated stop loss ends up being negative.
						sl = *boughtCoin.StopLoss
					}

					boughtCoin.StopLoss = &sl
					boughtCoin.TakeProfit = &tp

					b.sellLog.Infof("Price of %s reached more than the trading profit (TP). Adjusting stop loss (SL) to %.2f and trading profit (TP) to %.2f.", boughtCoin.Symbol, sl, tp)

					b.db.SaveOrder(boughtCoin)

					continue
				}

				// If the price of the coin is below the stop loss or above take profit then sell it.
				if currentPrice <= stopLoss || currentPrice >= takeProfit {
					var profitOrLossText string
					if priceChangePercentage >= 0 {
						profitOrLossText = "profit"
					} else {
						profitOrLossText = "loss"
					}

					estimatedProfitLoss := (currentPrice - buyPrice) * boughtCoin.Volume * (1 - (b.config.TradingOptions.TradingFeeMaker + b.config.TradingOptions.TradingFeeTaker))
					estimatedProfitLossWithFees := b.config.TradingOptions.Quantity * (priceChangePercentage - (b.config.TradingOptions.TradingFeeMaker + b.config.TradingOptions.TradingFeeTaker)) / 100
					msg := fmt.Sprintf(
						"Selling %.2f %s. Estimated %s: $%s %s%% (w/ fees: $%s %s%%)",
						boughtCoin.Volume,
						boughtCoin.Symbol,
						profitOrLossText,
						strconv.FormatFloat(estimatedProfitLoss, 'f', 2, 64),
						strconv.FormatFloat(priceChangePercentage, 'f', 2, 64),
						strconv.FormatFloat(estimatedProfitLossWithFees, 'f', 2, 64),
						strconv.FormatFloat(priceChangePercentage-(b.config.TradingOptions.TradingFeeMaker+b.config.TradingOptions.TradingFeeTaker), 'f', 2, 64),
					)

					b.sellLog.Infow(
						msg,
						"symbol", boughtCoin.Symbol,
						"buyPrice", buyPrice,
						"currentPrice", currentPrice,
						"priceChangePercentage", priceChangePercentage,
						"tradingFeeMaker", b.config.TradingOptions.TradingFeeMaker,
						"tradingFeeTaker", b.config.TradingOptions.TradingFeeTaker,
						"quantity", b.config.TradingOptions.Quantity,
						"testMode", b.config.EnableTestMode,
					)

					order := models.Order{
						Market:                b.market.Name(),
						Type:                  models.SellOrder,
						Volume:                boughtCoin.Volume,
						PriceChangePercentage: &priceChangePercentage,
						EstimatedProfitLoss:   &estimatedProfitLoss,
					}

					if b.config.EnableTestMode {
						order.Order = market.Order{
							OrderID:         0,
							Symbol:          boughtCoin.Symbol,
							TransactionTime: time.Now(),
							Price:           currentPrice,
						}
						order.IsTestMode = true
					} else {
						sellOrder, err := b.market.Sell(ctx, boughtCoin.Symbol, boughtCoin.Volume)
						if err != nil {
							b.sellLog.Errorf("Failed to sell %s: %s.", boughtCoin.Symbol, err)
							continue
						}

						order.Order = sellOrder
					}

					b.db.SaveOrder(order)
					b.db.DeleteOrder(boughtCoin)
				} else {
					b.sellLog.Infow(
						fmt.Sprintf("Price of %s is %.2f%% away from the buy price. Hodl.", boughtCoin.Symbol, priceChangePercentage),
						"symbol", boughtCoin.Symbol,
						"buyPrice", buyPrice,
						"currentPrice", currentPrice,
						"takeProfit", takeProfit,
						"stopLoss", stopLoss,
					)
				}
			}

			time.Sleep(time.Second * time.Duration(b.config.TradingOptions.SellTimeout))
		}
	}
}

// updateLatestCoins fetches the latest coins from the market and appends them to the volatilityWindow.
func (b *Bot) updateLatestCoins(ctx context.Context) error {
	b.botLog.Debug("Fetching latest coins.")

	coins, err := b.market.GetCoins(ctx)
	if err != nil {
		return err
	}

	b.volatilityWindow.AddRecord(coins)

	return nil
}

// convertVolume converts the volume given in the configured quantity from base currency (USDT) to each coin's volume.
func (b *Bot) convertVolume(ctx context.Context, quantity float64, volatileCoin market.VolatileCoin) (float64, error) {
	var stepSize float64

	// Get the step size of the coin from the local cache if it exists or from Binance if it doesn't (yet).
	// The step size never changes, so it's safe to cache it forever.
	// This approach avoids an additional API request to Binance per trade.
	cache, ok := b.db.GetCache(volatileCoin.Symbol)
	if ok {
		stepSize = cache.StepSize
	} else {
		info, err := b.market.GetSymbolInfo(ctx, volatileCoin.Symbol)
		if err != nil {
			return 0, err
		}

		stepSize = info.StepSize

		b.db.SaveCache(models.Cache{Symbol: volatileCoin.Symbol, StepSize: stepSize})
	}

	volume := quantity / volatileCoin.Price

	// Round the volume to the step size of the coin.
	if stepSize != 0 {
		volume = utils.RoundStepSize(volume, stepSize)
	}

	return volume, nil
}
