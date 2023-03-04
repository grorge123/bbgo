package momentum

import (
	"context"
	"fmt"

	"github.com/c9s/bbgo/pkg/fixedpoint"
	"github.com/c9s/bbgo/pkg/indicator"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/sirupsen/logrus"

	"github.com/c9s/bbgo/pkg/bbgo"
)

const ID = "momentum"

var log = logrus.WithField("strategy", ID)

func init() {
	// Register the pointer of the strategy struct,
	// so that bbgo knows what struct to be used to unmarshal the configs (YAML or JSON)
	// Note: built-in strategies need to imported manually in the bbgo cmd package.
	bbgo.RegisterStrategy(ID, &Strategy{})
}

type Strategy struct {
	Environment          *bbgo.Environment
	StandardIndicatorSet *bbgo.StandardIndicatorSet
	Market               types.Market

	// These fields will be filled from the config file (it translates YAML to JSON)
	Symbol string `json:"symbol"`

	// Interval is the interval used by the BOLLINGER indicator (which uses K-Line as its source price)
	types.IntervalWindow
	bbgo.QuantityOrAmount

	SmaSetting types.IntervalWindow `json:"sma"`
	EmaSetting types.IntervalWindow `json:"ema"`
	RsiSetting types.IntervalWindow `json:"rsi"`

	sma *indicator.SMA
	ema *indicator.EWMA
	rsi *indicator.RSI

	upCrossover   bool
	downCrossover bool

	takeProfit fixedpoint.Value
	stopLoss   fixedpoint.Value

	session *bbgo.ExchangeSession
	book    *types.StreamOrderBook

	ExitMethods bbgo.ExitMethodSet `json:"exits"`

	// persistence fields
	Position    *types.Position    `json:"position,omitempty" persistence:"position"`
	ProfitStats *types.ProfitStats `json:"profitStats,omitempty" persistence:"profit_stats"`

	orderExecutor *bbgo.GeneralOrderExecutor

	bbgo.StrategyController
}

func (s *Strategy) ID() string {
	return ID
}

func (s *Strategy) InstanceID() string {
	return fmt.Sprintf("%s:%s", ID, s.Symbol)
}

func (s *Strategy) Subscribe(session *bbgo.ExchangeSession) {
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{
		Interval: s.Interval,
	})

	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{
		Interval: s.SmaSetting.Interval,
	})
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{
		Interval: s.EmaSetting.Interval,
	})
	session.Subscribe(types.KLineChannel, s.Symbol, types.SubscribeOptions{
		Interval: s.RsiSetting.Interval,
	})

	s.ExitMethods.SetAndSubscribe(session, s)
}

func (s *Strategy) CurrentPosition() *types.Position {
	return s.Position
}

func (s *Strategy) ClosePosition(ctx context.Context, percentage fixedpoint.Value) error {
	if err := s.orderExecutor.GracefulCancel(ctx); err != nil {
		log.WithError(err).Errorf("graceful cancel order error")
	}
	s.stopLoss = 0
	s.takeProfit = 0
	return s.orderExecutor.ClosePosition(ctx, percentage)
}
func (s *Strategy) Run(ctx context.Context, orderExecutor bbgo.OrderExecutor, session *bbgo.ExchangeSession) error {
	s.session = session

	// StrategyController
	s.Status = types.StrategyStatusRunning
	if s.SmaSetting.Interval != s.EmaSetting.Interval {
		panic("SMA and EMA should have same interval")
	}
	s.sma = s.StandardIndicatorSet.SMA(s.SmaSetting)
	//s.ema = s.StandardIndicatorSet.EWMA(s.EmaSetting)
	ema := &indicator.EWMA{IntervalWindow: s.EmaSetting}
	s.rsi = s.StandardIndicatorSet.RSI(s.RsiSetting)
	store, ok := s.session.MarketDataStore(s.Symbol)
	if !ok {
		return fmt.Errorf("cannot get marketdatastore of %s", s.Symbol)
	}
	store.OnKLineWindowUpdate(func(interval types.Interval, window types.KLineWindow) {

		if s.EmaSetting.Interval == interval {
			idx := 0
			sum := 0.0
			for idx < s.EmaSetting.Window {
				sum += window.Close().Index(idx)
				idx += 1
			}
			avg := sum / float64(s.SmaSetting.Window)
			ema.Update(avg)
		}
	})
	s.ema = ema
	// If position is nil, we need to allocate a new position for calculation
	if s.Position == nil {
		s.Position = types.NewPositionFromMarket(s.Market)
	}

	if s.session.MakerFeeRate.Sign() > 0 || s.session.TakerFeeRate.Sign() > 0 {
		s.Position.SetExchangeFeeRate(s.session.ExchangeName, types.ExchangeFee{
			MakerFeeRate: s.session.MakerFeeRate,
			TakerFeeRate: s.session.TakerFeeRate,
		})
	}
	instanceID := s.InstanceID()

	if s.ProfitStats == nil {
		s.ProfitStats = types.NewProfitStats(s.Market)
	}

	// Always update the position fields
	s.Position.Strategy = ID
	s.Position.StrategyInstanceID = instanceID

	s.orderExecutor = bbgo.NewGeneralOrderExecutor(session, s.Symbol, ID, instanceID, s.Position)
	s.orderExecutor.BindEnvironment(s.Environment)
	s.orderExecutor.BindProfitStats(s.ProfitStats)
	s.orderExecutor.Bind()
	s.orderExecutor.TradeCollector().OnPositionUpdate(func(position *types.Position) {
		bbgo.Sync(ctx, s)
	})
	s.ExitMethods.Bind(session, s.orderExecutor)

	s.OnSuspend(func() {
		_ = s.orderExecutor.GracefulCancel(ctx)
		bbgo.Sync(ctx, s)
	})

	s.OnEmergencyStop(func() {
		// Close 100% position
		percentage := fixedpoint.NewFromFloat(1.0)
		_ = s.ClosePosition(ctx, percentage)
	})

	session.MarketDataStream.OnKLineClosed(types.KLineWith(s.Symbol, s.Interval, func(kline types.KLine) {
		ema := s.ema.Last()
		sma := s.sma.Last()
		rsi := s.rsi.Last()
		price, ok := session.LastPrice(s.Symbol)
		if !ok {
			log.Errorf("Can not get new price. %v", ok)
		}
		log.Infof("Time: %v, EMA: %.4f, SMA: %.4f, RSI: %.4f, open: %v, high: %v, low: %v, close: %v",
			kline.StartTime, ema, sma, rsi, kline.Open, kline.High, kline.Low, kline.Close)
		if kline.Close.Float64() > ema {
			s.upCrossover = true
		} else if kline.Close.Float64() < ema {
			s.downCrossover = true
		}
		if sma > ema && inBetween(rsi, 50, 70) && kline.Low.Float64() > sma && kline.Close.Compare(kline.Open) > 0 && s.downCrossover == true {
			if !s.Position.IsOpened(price) {
				log.Warnf("long at %v, position %v", kline.Close, s.Position.GetBase())
				if err := s.orderExecutor.GracefulCancel(ctx); err != nil {
					log.WithError(err).Errorf("graceful cancel order error")
				}
				_, err := s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
					Symbol:           s.Symbol,
					Side:             types.SideTypeBuy,
					Type:             types.OrderTypeMarket,
					Quantity:         s.Quantity,
					MarginSideEffect: types.SideEffectTypeMarginBuy,
					Tag:              "Long order",
				})
				if err == nil {
					s.stopLoss = kline.Low
					s.takeProfit = kline.Close.Add(kline.Close.Sub(kline.Low).Mul(fixedpoint.NewFromFloat(1.5)))
					_, err := s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
						Symbol:   s.Symbol,
						Side:     types.SideTypeSell,
						Type:     types.OrderTypeLimit,
						Price:    s.takeProfit,
						Quantity: s.Quantity,
						// MarginSideEffect: types.SideEffectTypeAutoRepay,
						Tag: "Long TP",
					})
					if err != nil {
						log.Errorln(err)
					}
				}
				if err != nil {
					log.Errorln(err)
				}
			}
			s.downCrossover = false
		} else if sma < ema && inBetween(rsi, 30, 50) && kline.High.Float64() < sma && kline.Close.Compare(kline.Open) < 0 && s.upCrossover == true {
			if !s.Position.IsOpened(price) {
				log.Warnf("Short at %v, position %v", kline.Close, s.Position.GetBase())
				if err := s.orderExecutor.GracefulCancel(ctx); err != nil {
					log.WithError(err).Errorf("graceful cancel order error")
				}
				_, err := s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
					Symbol:           s.Symbol,
					Side:             types.SideTypeSell,
					Type:             types.OrderTypeMarket,
					Quantity:         s.Quantity,
					MarginSideEffect: types.SideEffectTypeMarginBuy,
					Tag:              "Short order",
				})
				if err == nil {
					s.stopLoss = kline.High
					s.takeProfit = kline.Close.Sub(kline.High.Sub(kline.Close).Mul(fixedpoint.NewFromFloat(1.5)))
					_, err := s.orderExecutor.SubmitOrders(ctx, types.SubmitOrder{
						Symbol:   s.Symbol,
						Side:     types.SideTypeBuy,
						Type:     types.OrderTypeLimit,
						Price:    s.takeProfit,
						Quantity: s.Quantity,
						// MarginSideEffect: types.SideEffectTypeAutoRepay,
						Tag: "Short TP",
					})
					if err != nil {
						log.Errorln(err)
					}
				}
				if err != nil {
					log.Errorln(err)
				}
			}
			s.upCrossover = false
		}
		exit := false
		if s.Position.IsLong() && price < s.stopLoss {
			exit = true
		} else if s.Position.IsShort() && price > s.stopLoss {
			exit = true
		}
		if exit {
			if err := s.orderExecutor.GracefulCancel(ctx); err != nil {
				log.WithError(err).Errorf("graceful cancel order error")
			}
			log.Warnf("Close position %v, at %v", s.Position.GetBase(), price)
			s.orderExecutor.ClosePosition(ctx, fixedpoint.One, "close position by SL")
			s.takeProfit = 0
			s.stopLoss = 0
		}
	}))

	return nil
}

func inBetween(x, a, b float64) bool {
	return a < x && x < b
}
