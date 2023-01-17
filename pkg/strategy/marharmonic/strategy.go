package marharmonic

import (
	"context"
	"fmt"
	"math"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/data/tsv"
	"github.com/c9s/bbgo/pkg/datatype/floats"
	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/indicator"
	"github.com/c9s/bbgo/pkg/types"

	"github.com/sirupsen/logrus"
)

const ID = "marharmonic"

var log = logrus.WithField("strategy", ID)

func init() {
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type Strategy struct {
	Environment *bbgo.Environment
	Symbol      string `json:"symbol"`
	Market      types.Market

	types.IntervalWindow
	//bbgo.OpenPositionOptions

	// persistence fields
	Position    *types.Position    `persistence:"position"`
	ProfitStats *types.ProfitStats `persistence:"profit_stats"`
	TradeStats  *types.TradeStats  `persistence:"trade_stats"`

	ExitMethods bbgo.ExitMethodSet `json:"exits"`

	session       *bbgo.ExchangeSession
	orderExecutor *bbgo.GeneralOrderExecutor

	bbgo.QuantityOrAmount

	// StrategyController
	bbgo.StrategyController

	shark *SHARK

	AccountValueCalculator *bbgo.AccountValueCalculator

	// whether to draw graph or not by the end of backtest
	DrawGraph               bool             `json:"drawGraph"`
	GraphPNLPath            string           `json:"graphPNLPath"`
	GraphCumPNLPath         string           `json:"graphCumPNLPath"`
	LowFilter               float64          `json:"lowFilter"`
	HighFilter              float64          `json:"highFilter"`
	Leverage                fixedpoint.Value `json:"leverage"`
	TrailingCallbackRate    []float64        `json:"trailingCallbackRate" modifiable:"true"`
	TrailingActivationRatio []float64        `json:"trailingActivationRatio" modifiable:"true"`

	// for position
	BuyPrice     float64 `persistence:"buy_price"`
	SellPrice    float64 `persistence:"sell_price"`
	HighestPrice float64 `persistence:"highest_price"`
	LowestPrice  float64 `persistence:"lowest_price"`
	HighestShark float64 `persistence:"highest_shark"`
	LowestShark  float64 `persistence:"lowest_shark"`

	// Accumulated profit report
	AccumulatedProfitReport *AccumulatedProfitReport `json:"accumulatedProfitReport"`
}

// AccumulatedProfitReport For accumulated profit report output
type AccumulatedProfitReport struct {
	// AccumulatedProfitMAWindow Accumulated profit SMA window, in number of trades
	AccumulatedProfitMAWindow int `json:"accumulatedProfitMAWindow"`

	// IntervalWindow interval window, in days
	IntervalWindow int `json:"intervalWindow"`

	// NumberOfInterval How many intervals to output to TSV
	NumberOfInterval int `json:"NumberOfInterval"`

	// TsvReportPath The path to output report to
	TsvReportPath string `json:"tsvReportPath"`

	// AccumulatedDailyProfitWindow The window to sum up the daily profit, in days
	AccumulatedDailyProfitWindow int `json:"accumulatedDailyProfitWindow"`

	// Accumulated profit
	accumulatedProfit         fixedpoint.Value
	accumulatedProfitPerDay   floats.Slice
	previousAccumulatedProfit fixedpoint.Value

	// Accumulated profit MA
	accumulatedProfitMA       *indicator.SMA
	accumulatedProfitMAPerDay floats.Slice

	// Daily profit
	dailyProfit floats.Slice

	// Accumulated fee
	accumulatedFee       fixedpoint.Value
	accumulatedFeePerDay floats.Slice

	// Win ratio
	winRatioPerDay floats.Slice

	// Profit factor
	profitFactorPerDay floats.Slice

	// Trade number
	dailyTrades               floats.Slice
	accumulatedTrades         int
	previousAccumulatedTrades int
}

func (r *AccumulatedProfitReport) Initialize() {
	if r.AccumulatedProfitMAWindow <= 0 {
		r.AccumulatedProfitMAWindow = 60
	}
	if r.IntervalWindow <= 0 {
		r.IntervalWindow = 7
	}
	if r.AccumulatedDailyProfitWindow <= 0 {
		r.AccumulatedDailyProfitWindow = 7
	}
	if r.NumberOfInterval <= 0 {
		r.NumberOfInterval = 1
	}
	r.accumulatedProfitMA = &indicator.SMA{IntervalWindow: types.IntervalWindow{Interval: types.Interval1d, Window: r.AccumulatedProfitMAWindow}}
}

func (r *AccumulatedProfitReport) RecordProfit(profit fixedpoint.Value) {
	r.accumulatedProfit = r.accumulatedProfit.Add(profit)
}

func (r *AccumulatedProfitReport) RecordTrade(fee fixedpoint.Value) {
	r.accumulatedFee = r.accumulatedFee.Add(fee)
	r.accumulatedTrades += 1
}

func (r *AccumulatedProfitReport) DailyUpdate(tradeStats *types.TradeStats) {
	// Daily profit
	r.dailyProfit.Update(r.accumulatedProfit.Sub(r.previousAccumulatedProfit).Float64())
	r.previousAccumulatedProfit = r.accumulatedProfit

	// Accumulated profit
	r.accumulatedProfitPerDay.Update(r.accumulatedProfit.Float64())

	// Accumulated profit MA
	r.accumulatedProfitMA.Update(r.accumulatedProfit.Float64())
	r.accumulatedProfitMAPerDay.Update(r.accumulatedProfitMA.Last())

	// Accumulated Fee
	r.accumulatedFeePerDay.Update(r.accumulatedFee.Float64())

	// Win ratio
	r.winRatioPerDay.Update(tradeStats.WinningRatio.Float64())

	// Profit factor
	r.profitFactorPerDay.Update(tradeStats.ProfitFactor.Float64())

	// Daily trades
	r.dailyTrades.Update(float64(r.accumulatedTrades - r.previousAccumulatedTrades))
	r.previousAccumulatedTrades = r.accumulatedTrades
}

// Output Accumulated profit report to a TSV file
func (r *AccumulatedProfitReport) Output(symbol string) {
	if r.TsvReportPath != "" {
		tsvwiter, err := tsv.AppendWriterFile(r.TsvReportPath)
		if err != nil {
			panic(err)
		}
		defer tsvwiter.Close()
		// Output symbol, total acc. profit, acc. profit 60MA, interval acc. profit, fee, win rate, profit factor
		_ = tsvwiter.Write([]string{"#", "Symbol", "accumulatedProfit", "accumulatedProfitMA", fmt.Sprintf("%dd profit", r.AccumulatedDailyProfitWindow), "accumulatedFee", "winRatio", "profitFactor", "60D trades"})
		for i := 0; i <= r.NumberOfInterval-1; i++ {
			accumulatedProfit := r.accumulatedProfitPerDay.Index(r.IntervalWindow * i)
			accumulatedProfitStr := fmt.Sprintf("%f", accumulatedProfit)
			accumulatedProfitMA := r.accumulatedProfitMAPerDay.Index(r.IntervalWindow * i)
			accumulatedProfitMAStr := fmt.Sprintf("%f", accumulatedProfitMA)
			intervalAccumulatedProfit := r.dailyProfit.Tail(r.AccumulatedDailyProfitWindow+r.IntervalWindow*i).Sum() - r.dailyProfit.Tail(r.IntervalWindow*i).Sum()
			intervalAccumulatedProfitStr := fmt.Sprintf("%f", intervalAccumulatedProfit)
			accumulatedFee := fmt.Sprintf("%f", r.accumulatedFeePerDay.Index(r.IntervalWindow*i))
			winRatio := fmt.Sprintf("%f", r.winRatioPerDay.Index(r.IntervalWindow*i))
			profitFactor := fmt.Sprintf("%f", r.profitFactorPerDay.Index(r.IntervalWindow*i))
			trades := r.dailyTrades.Tail(60+r.IntervalWindow*i).Sum() - r.dailyTrades.Tail(r.IntervalWindow*i).Sum()
			tradesStr := fmt.Sprintf("%f", trades)

			_ = tsvwiter.Write([]string{fmt.Sprintf("%d", i+1), symbol, accumulatedProfitStr, accumulatedProfitMAStr, intervalAccumulatedProfitStr, accumulatedFee, winRatio, profitFactor, tradesStr})
		}
	}
}

func (s *Strategy) Subscribe(session *bbgo.ExchangeSession) {
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{Interval: s.Interval})

	if !bbgo.IsBackTesting {
		session.Subscribe(types.MarketTradeChannel, s.Symbol, types.SubscribeOptions{})
	}

	s.ExitMethods.SetAndSubscribe(session, s)
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	return fmt.Sprintf("%s:%s", ID, s.Symbol)
}

func (s *Strategy) CalcAssetValue(price fixedpoint.Value) fixedpoint.Value {
	balances := s.session.GetAccount().Balances()
	return balances[s.Market.BaseCurrency].Total().Mul(price).Add(balances[s.Market.QuoteCurrency].Total())
}

func (s *Strategy) CurrentPosition() *types.Position {
	return s.Position
}

func (s *Strategy) ClosePosition(ctx context.Context, percentage fixedpoint.Value) error {
	if err := s.orderExecutor.GracefulCancel(ctx); err != nil {
		log.WithError(err).Errorf("graceful cancel order error")
	}
	s.LowestShark = 0
	s.HighestShark = 0
	return s.orderExecutor.ClosePosition(ctx, percentage)
}

func (s *Strategy) trailingCheck(price float64, direction string) bool {
	if s.HighestPrice > 0 && s.HighestPrice < price {
		s.HighestPrice = price
	}
	if s.LowestPrice > 0 && s.LowestPrice > price {
		s.LowestPrice = price
	}
	isShort := direction == "short"
	if isShort && s.LowestShark == 0 || !isShort && s.HighestShark == 0 {
		return false
	}
	for i := len(s.TrailingCallbackRate) - 1; i >= 0; i-- {
		trailingCallbackRate := s.TrailingCallbackRate[i]
		trailingActivationRatio := s.TrailingActivationRatio[i]
		if isShort {
			if (s.LowestShark-s.LowestPrice)/s.LowestShark > trailingActivationRatio {
				return (price-s.LowestPrice)/s.LowestShark > trailingCallbackRate
			}
		} else {
			if (s.HighestPrice-s.HighestShark)/s.HighestShark > trailingActivationRatio {
				return (s.HighestPrice-price)/s.HighestShark > trailingCallbackRate
			}
		}
	}
	return false
}
func (s *Strategy) calculateAvailable(ctx context.Context, currentPrice fixedpoint.Value, side types.SideType, leverage fixedpoint.Value) fixedpoint.Value {
	quoteQty, err := bbgo.CalculateQuoteQuantity(ctx, s.session, s.Market.QuoteCurrency, leverage)
	if err != nil {
		log.WithError(err).Errorf("can not update %s quote balance from exchange", s.Symbol)
		return fixedpoint.Zero
	}
	BaseCurrencyBalance, ok := s.session.GetAccount().Balance(s.Market.BaseCurrency)
	if !ok {
		log.WithError(err).Errorf("can not get Account")
		return fixedpoint.Zero
	}
	QuoteCurrencyBalance, ok := s.session.GetAccount().Balance(s.Market.QuoteCurrency)
	if !ok {
		log.WithError(err).Errorf("can not get Account")
		return fixedpoint.Zero
	}
	baseQuoteCurrencyBalance := QuoteCurrencyBalance.Total().Div(currentPrice)
	baseQty := quoteQty.Div(currentPrice)
	// log.Infof("currentPrice %v, quoteQty: %v, BaseCurrencyBalance: %v, QuoteCurrencyBalance: %v, baseQty: %v, baseQuoteCurrencyBalance: %v",
	// currentPrice, quoteQty, BaseCurrencyBalance.Total(), QuoteCurrencyBalance.Total(), baseQty, baseQuoteCurrencyBalance)
	if side == types.SideTypeSell {
		return baseQty.Sub(baseQuoteCurrencyBalance)
	} else {
		return baseQty.Sub(BaseCurrencyBalance.Total())
	}
}
func (s *Strategy) Run(ctx context.Context, orderExecutor bbgo.OrderExecutor, session *bbgo.ExchangeSession) error {
	var instanceID = s.InstanceID()

	if s.Position == nil {
		s.Position = types.NewPositionFromMarket(s.Market)
	}

	if s.ProfitStats == nil {
		s.ProfitStats = types.NewProfitStats(s.Market)
	}

	if s.TradeStats == nil {
		s.TradeStats = types.NewTradeStats(s.Symbol)
	}
	// StrategyController
	s.Status = types.StrategyStatusRunning

	s.OnSuspend(func() {
		// Cancel active orders
		_ = s.orderExecutor.GracefulCancel(ctx)
		bbgo.Sync(ctx, s)
	})

	s.OnEmergencyStop(func() {
		// Cancel active orders
		_ = s.orderExecutor.GracefulCancel(ctx)
		// Close 100% position
		//_ = s.ClosePosition(ctx, fixedpoint.One)
	})

	s.session = session

	// Set fee rate
	if s.session.MakerFeeRate.Sign() > 0 || s.session.TakerFeeRate.Sign() > 0 {
		s.Position.SetExchangeFeeRate(s.session.ExchangeName, types.ExchangeFee{
			MakerFeeRate: s.session.MakerFeeRate,
			TakerFeeRate: s.session.TakerFeeRate,
		})
	}

	s.orderExecutor = bbgo.NewGeneralOrderExecutor(session, s.Symbol, ID, instanceID, s.Position)
	s.orderExecutor.BindEnvironment(s.Environment)
	s.orderExecutor.BindProfitStats(s.ProfitStats)
	s.orderExecutor.BindTradeStats(s.TradeStats)

	// AccountValueCalculator
	s.AccountValueCalculator = bbgo.NewAccountValueCalculator(s.session, s.Market.QuoteCurrency)

	// Accumulated profit report
	if bbgo.IsBackTesting {
		if s.AccumulatedProfitReport == nil {
			s.AccumulatedProfitReport = &AccumulatedProfitReport{}
		}
		s.AccumulatedProfitReport.Initialize()
		s.orderExecutor.TradeCollector().OnProfit(func(trade types.Trade, profit *types.Profit) {
			if profit == nil {
				return
			}

			s.AccumulatedProfitReport.RecordProfit(profit.Profit)
		})
		// s.orderExecutor.TradeCollector().OnTrade(func(trade types.Trade, profit fixedpoint.Value, netProfit fixedpoint.Value) {
		// 	s.AccumulatedProfitReport.RecordTrade(trade.Fee)
		// })
		session.MarketDataStream.OnKLineClosed(types.KLineWith(s.Symbol, types.Interval1d, func(kline types.KLine) {
			s.AccumulatedProfitReport.DailyUpdate(s.TradeStats)
		}))
	}

	// For drawing
	profitSlice := floats.Slice{1., 1.}
	price, _ := session.LastPrice(s.Symbol)
	initAsset := s.CalcAssetValue(price).Float64()
	cumProfitSlice := floats.Slice{initAsset, initAsset}

	s.orderExecutor.TradeCollector().OnTrade(func(trade types.Trade, profit fixedpoint.Value, netProfit fixedpoint.Value) {
		if bbgo.IsBackTesting {
			s.AccumulatedProfitReport.RecordTrade(trade.Fee)
		}

		// For drawing/charting
		price := trade.Price.Float64()
		if s.BuyPrice > 0 {
			profitSlice.Update(price / s.BuyPrice)
			cumProfitSlice.Update(s.CalcAssetValue(trade.Price).Float64())
		} else if s.SellPrice > 0 {
			profitSlice.Update(s.SellPrice / price)
			cumProfitSlice.Update(s.CalcAssetValue(trade.Price).Float64())
		}
		if s.Position.IsDust(trade.Price) {
			s.BuyPrice = 0
			s.SellPrice = 0
			s.HighestPrice = 0
			s.LowestPrice = 0
		} else if s.Position.IsLong() {
			s.BuyPrice = price
			s.SellPrice = 0
			s.HighestPrice = s.BuyPrice
			s.LowestPrice = 0
		} else {
			s.SellPrice = price
			s.BuyPrice = 0
			s.HighestPrice = 0
			s.LowestPrice = s.SellPrice
		}
	})

	s.orderExecutor.TradeCollector().OnPositionUpdate(func(position *types.Position) {
		bbgo.Sync(ctx, s)
	})
	s.orderExecutor.Bind()

	for _, method := range s.ExitMethods {
		method.Bind(session, s.orderExecutor)
	}

	kLineStore, _ := s.session.MarketDataStore(s.Symbol)
	s.shark = &SHARK{IntervalWindow: types.IntervalWindow{Window: s.Window, Interval: s.Interval}}
	s.shark.BindK(s.session.MarketDataStream, s.Symbol, s.shark.Interval)
	if klines, ok := kLineStore.KLinesOfInterval(s.shark.Interval); ok {
		s.shark.LoadK((*klines)[0:])
	}
	s.session.MarketDataStream.OnKLineClosed(types.KLineWith(s.Symbol, s.Interval, func(kline types.KLine) {

		price, ok := s.session.LastPrice(s.Symbol)
		if !ok {
			log.Error("cannot get lastprice")
		}
		pricef := price.Float64()
		lowf := math.Min(kline.Low.Float64(), pricef)
		highf := math.Max(kline.High.Float64(), pricef)
		if s.LowestPrice > 0 && lowf < s.LowestPrice {
			s.LowestPrice = lowf
		}
		if s.HighestPrice > 0 && highf > s.HighestPrice {
			s.HighestPrice = highf
		}

		if s.Position.IsLong() {
			if s.HighestShark != 0 && pricef >= s.HighestShark {
				log.Errorf("price: %v upper than shark: %v", pricef, s.HighestShark)
			}
		} else if s.Position.IsShort() {
			if s.LowestShark != 0 && pricef <= s.LowestShark {
				log.Errorf("price: %v lower than shark: %v", pricef, s.LowestShark)
			}
		}
		log.Infof("Shark Score: %f, Current Price: %f", s.shark.Last(), kline.Close.Float64())

		// previousRegime := s.shark.Values.Tail(10).Mean()
		// zeroThreshold := 5.
		if s.HighFilter == 0 {
			s.HighFilter = 0.99
		}
		if s.LowFilter == 0 {
			s.LowFilter = 0.01
		}
		if s.shark.Rank(s.Window).Last()/float64(s.Window) > s.HighFilter { // && ((previousRegime < zeroThreshold && previousRegime > -zeroThreshold) || s.shark.Index(1) < 0) {
			if s.Position.IsShort() {

				log.Warnf("Close short position instead long")
				if err := s.orderExecutor.GracefulCancel(ctx); err != nil {
					log.WithError(err).Errorf("graceful cancel order error")
				}
				s.orderExecutor.ClosePosition(ctx, fixedpoint.One, "close short position")
				s.LowestShark = 0
			}
			available := s.calculateAvailable(ctx, price, types.SideTypeBuy, s.Leverage)
			// log.Warnf("Long available: %v", available)
			if available.Compare(s.Quantity) >= 0 {
				log.Warnf("long at %v, position %v, IsShort %t", price, s.Position.GetBase(), s.Position.IsShort())
				_, err := s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
					Symbol:           s.Symbol,
					Side:             types.SideTypeBuy,
					Type:             types.OrderTypeMarket,
					Quantity:         s.Quantity,
					MarginSideEffect: types.SideEffectTypeMarginBuy,
					Tag:              "shark long: buy in",
				})
				if err == nil {
					// 	_, err = s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
					// 		Symbol:           s.Symbol,
					// 		Side:             types.SideTypeSell,
					// 		Quantity:         s.Quantity,
					// 		Price:            fixedpoint.NewFromFloat(s.shark.Highs.Tail(100).Max()),
					// 		Type:             types.OrderTypeLimit,
					// 		MarginSideEffect: types.SideEffectTypeAutoRepay,
					// 		Tag:              "shark long: sell back",
					// 	})
					if s.HighestShark == 0 {
						s.HighestShark = s.shark.Highs.Tail(100).Max()
					} else {
						s.HighestShark = math.Max(s.HighestShark, s.shark.Highs.Tail(100).Max())
					}
					log.Warnf("Update highest Shark: %v, now shark: %v", s.HighestShark, s.shark.Highs.Tail(100).Max())
				}
				if err != nil {
					log.Errorln(err)
				}
			} else {
				log.Warnf("Have no enough money to Buy")
			}

		} else if s.shark.Rank(s.Window).Last()/float64(s.Window) < s.LowFilter { // && ((previousRegime < zeroThreshold && previousRegime > -zeroThreshold) || s.shark.Index(1) > 0) {
			if s.Position.IsLong() {
				log.Warnf("Close long position instead short")
				if err := s.orderExecutor.GracefulCancel(ctx); err != nil {
					log.WithError(err).Errorf("graceful cancel order error")
				}
				s.orderExecutor.ClosePosition(ctx, fixedpoint.One, "close long position")
				s.HighestShark = 0
			}
			available := s.calculateAvailable(ctx, price, types.SideTypeSell, s.Leverage)
			// log.Warnf("Short available: %v", available)
			if available.Compare(s.Quantity) >= 0 {
				log.Warnf("Short at %v, position %v, IsLong %t", price, s.Position.GetBase(), s.Position.IsLong())
				_, err := s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
					Symbol:           s.Symbol,
					Side:             types.SideTypeSell,
					Quantity:         s.Quantity,
					Type:             types.OrderTypeMarket,
					MarginSideEffect: types.SideEffectTypeMarginBuy,
					Tag:              "shark short: sell in",
				})
				if err == nil {
					// 	_, err = s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
					// 		Symbol:           s.Symbol,
					// 		Side:             types.SideTypeBuy,
					// 		Quantity:         s.Quantity,
					// 		Price:            fixedpoint.NewFromFloat(s.shark.Lows.Tail(100).Min()),
					// 		Type:             types.OrderTypeLimit,
					// 		MarginSideEffect: types.SideEffectTypeAutoRepay,
					// 		Tag:              "shark short: buy back",
					// 	})
					if s.LowestShark == 0 {
						s.LowestShark = s.shark.Lows.Tail(100).Min()
					} else {
						s.LowestShark = math.Min(s.LowestShark, s.shark.Lows.Tail(100).Min())
					}
					log.Warnf("Update lowest Shark: %v, now shark: %v", s.LowestShark, s.shark.Lows.Tail(100).Min())
				}
				if err != nil {
					log.Errorln(err)
				}
			} else {
				log.Warnf("Have no enough money to short")
			}
		}
		log.Warnf("Now price: %v, highf: %v, lowf: %v, LowestShark: %v, HighestShark: %v, LowestPrice: %v, HighestPrice: %v", pricef, highf, lowf, s.LowestShark, s.HighestShark, s.LowestPrice, s.HighestPrice)
		exitCondition := s.trailingCheck(pricef, "short") || s.trailingCheck(pricef, "long")
		if exitCondition {
			log.Warnf("Close position %v, at %v", s.Position.GetBase(), price)
			s.orderExecutor.ClosePosition(ctx, fixedpoint.One, "close position by trailing")
			s.LowestShark = 0
			s.HighestShark = 0
		}
	}))

	return nil
}
