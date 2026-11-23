// Package network implements the simulated WAN: time-varying per-link quality
// models (latency, loss, jitter, bandwidth), outage injection, the hub-and-spoke
// topology of stores/regional warehouses, and the packet-level link queues.
package network

import (
	"math"
	"math/rand"
	"time"

	"sdwan/internal/model"
)

// LinkConfig captures the static characteristics of one WAN link.
type LinkConfig struct {
	ID       model.LinkID
	Type     model.LinkType
	BaseRTT  float64 // 毫秒
	BaseJit  float64 // 毫秒
	BaseLoss float64 // 0..1
	UpBps    float64
	DownBps  float64
	Seed     int64
}

// LinkModel is a deterministic, time-varying quality model for one link.
// Everything derives from a per-link seeded RNG so whole simulations are
// reproducible.
type LinkModel struct {
	cfg LinkConfig
	rng *rand.Rand

	// OU 噪声状态（均值回归），分别作用于时延和丢包
	noiseRTT  float64
	noiseLoss float64

	outage bool

	// 容量覆盖（故障注入用）：0 表示无覆盖，使用配置原值
	upOverride   float64
	downOverride float64
}

// NewLink constructs a link model from its static config.
func NewLink(cfg LinkConfig) *LinkModel {
	return &LinkModel{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}
}

func (m *LinkModel) Config() LinkConfig   { return m.cfg }
func (m *LinkModel) Type() model.LinkType { return m.cfg.Type }
func (m *LinkModel) ID() model.LinkID     { return m.cfg.ID }

// SetOutage injects/clears a hard link fault (broadband cut etc.).
func (m *LinkModel) SetOutage(down bool) { m.outage = down }
func (m *LinkModel) Outage() bool        { return m.outage }

// SetConfig hot-updates static link parameters (used by fault injection to
// temporarily squeeze capacity, then restore it).
func (m *LinkModel) SetConfig(cfg LinkConfig) { m.cfg = cfg }

// SetCapacityOverride multiplies current configured capacity by mult; the
// original config is retained and restored by ClearCapacityOverride. Repeated
// overrides compose from the original config, not the already-squeezed value.
func (m *LinkModel) SetCapacityOverride(mult float64) {
	m.upOverride = m.cfg.UpBps * mult
	m.downOverride = m.cfg.DownBps * mult
}

// ClearCapacityOverride removes a capacity override.
func (m *LinkModel) ClearCapacityOverride() {
	m.upOverride, m.downOverride = 0, 0
}

// eveningFactor returns 0..1, peaking around 21:30, for 19:00-24:00.
func eveningFactor(hour float64) float64 {
	if hour < 19 || hour >= 24 {
		return 0
	}
	return math.Sin(math.Pi * (hour - 19) / 5)
}

// hourOf converts sim time to fractional hour.
func hourOf(t time.Time) float64 {
	return float64(t.Hour()) + float64(t.Minute())/60 + float64(t.Second())/3600
}

// stepOU advances one mean-reverting Ornstein-Uhlenbeck step.
func stepOU(rng *rand.Rand, x float64, theta, sigma float64) float64 {
	return x*(1-theta) + sigma*rng.NormFloat64()
}

// dialFactor returns technology-specific time-of-day multipliers.
// RTT/jitter get larger, loss gets additive, capacity shrinks.
func dialFactor(typ model.LinkType, hour float64) (rtt, jit, loss, capMult float64) {
	e := eveningFactor(hour)
	switch typ {
	case model.LinkBroadband:
		// 家用宽带晚高峰：晚 7 点到午夜显著劣化
		return 1 + 1.3*e, 1 + 2.0*e, 0.07 * e, 1 - 0.55*e
	case model.LinkCellular:
		// 移动网晚高峰也有一定劣化但比宽带轻
		return 1 + 0.6*e, 1 + e, 0.03 * e, 1 - 0.3*e
	case model.LinkMPLS:
		// 专线白天营业时段有轻度拥塞基线
		biz := 0.0
		if hour >= 9 && hour < 21 {
			biz = 0.25 * math.Sin(math.Pi*(hour-9)/12)
		}
		return 1 + biz, 1 + biz*0.5, 0.002 * biz, 1
	}
	return 1, 1, 0, 1
}

// Sample returns the link quality at virtual time now given current
// utilization util (0..1) of the link capacity. Used by the fluid engine and
// by the probe tracker in packet mode.
func (m *LinkModel) Sample(now time.Time, util float64) model.Quality {
	if m.outage {
		return model.Quality{Healthy: false, UpBps: m.cfg.UpBps, DownBps: m.cfg.DownBps}
	}
	hour := hourOf(now)
	rttF, jitF, lossAdd, capMult := dialFactor(m.cfg.Type, hour)

	// 每秒一步的 OU 噪声（Sample 按秒调用）
	m.noiseRTT = stepOU(m.rng, m.noiseRTT, 0.08, 0.12)
	m.noiseLoss = stepOU(m.rng, m.noiseLoss, 0.1, 0.05)

	u := math.Max(0, math.Min(0.999, util))
	// 排队时延随利用率非线性上升
	qDelay := 3.0 * u / (1.02 - u)
	congLoss := math.Max(0, 0.45*(u-0.92))

	base := m.cfg.BaseRTT * rttF
	rtt := base*(1+m.noiseRTT) + qDelay
	jitter := m.cfg.BaseJit*jitF + math.Abs(m.noiseRTT)*base*0.15 + qDelay*0.3
	loss := math.Min(0.6, m.cfg.BaseLoss+lossAdd+congLoss+m.noiseLoss*0.01)

	up, down := m.cfg.UpBps, m.cfg.DownBps
	if m.upOverride > 0 {
		up, down = m.upOverride, m.downOverride
	}
	return model.Quality{
		RTT:     math.Max(1, rtt),
		Loss:    math.Max(0, loss),
		Jitter:  math.Max(0, jitter),
		UpBps:   up * capMult,
		DownBps: down * capMult,
		Util:    util,
		Healthy: true,
	}
}

// OneWayDelay draws a randomized one-way forwarding delay for a packet
// (packet-mode engine). Includes jitter and current queuing delay.
func (m *LinkModel) OneWayDelay(now time.Time, util float64) time.Duration {
	q := m.Sample(now, util)
	half := q.RTT/2 + m.rng.NormFloat64()*q.Jitter*0.5
	return time.Duration(math.Max(0.2, half)) * time.Millisecond
}

// RandomDrop decides per-packet random loss (tail drop from queue overflow is
// handled separately by the packet engine).
func (m *LinkModel) RandomDrop(now time.Time, util float64) bool {
	if m.outage {
		return true
	}
	q := m.Sample(now, util)
	return m.rng.Float64() < q.Loss
}
