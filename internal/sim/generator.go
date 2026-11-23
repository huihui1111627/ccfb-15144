// Package sim contains the simulation machinery: store traffic generators with
// realistic per-application behavior, deterministic fault injection, the
// fluid engine for multi-day acceptance runs, and a packet-level engine for
// the live dashboard.
package sim

import (
	"fmt"
	"math/rand"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/network"
)

// FlowDemand is one flow's offered load in a simulated second.
type FlowDemand struct {
	Key     model.FlowKey
	App     model.AppID // ground truth from the generator
	Dst     model.Endpoint
	Bytes   int64 // 本 tick 期望上行字节
	First   bool
	Payload []byte
	// 行为观察（供行为归类用）
	PktCount  int
	MeanPkt   int
	IntervalS float64
	Ending    bool // 本 tick 后流结束
}

// flowGen holds a generator's live state for one flow.
type flowGen struct {
	key      model.FlowKey
	app      model.AppID
	dst      model.Endpoint
	durTicks int
	age      int
	bps      float64
	pktSize  int
	interval float64 // 秒，0 = 按字节填充
	rng      *rand.Rand
	first    bool
	payload  []byte
}

// TrafficGenerator produces per-store application flows with diurnal patterns.
type TrafficGenerator struct {
	store  *network.Store
	hub    *network.Hub
	rng    *rand.Rand
	active []*flowGen
	tick   int64
	// 每应用每 tick 新流概率的缩放，由全局负载配置控制
	loadMul float64
}

// GeneratorConfig controls offered load per store.
type GeneratorConfig struct {
	LoadMul float64 // 1.0 = 默认负载
}

// NewGenerator builds a generator for a store.
func NewGenerator(st *network.Store, hub *network.Hub, seed int64, cfg GeneratorConfig) *TrafficGenerator {
	mul := cfg.LoadMul
	if mul <= 0 {
		mul = 1
	}
	return &TrafficGenerator{store: st, hub: hub, rng: rand.New(rand.NewSource(seed)), loadMul: mul}
}

// diurnal returns a 0.15..1 activity multiplier following business hours.
func diurnal(hour float64) float64 {
	switch {
	case hour >= 9 && hour <= 21:
		return 0.55 + 0.45*sinBump((hour-9)/12)
	case hour >= 7 && hour < 9:
		return 0.15 + 0.4*(hour-7)/2
	case hour > 21 && hour < 23:
		return 0.55 - 0.4*(hour-21)/2
	default:
		// 夜间只有备份/更新活跃
		return 0.12
	}
}

func sinBump(x float64) float64 { return 1 - (x-0.5)*(x-0.5)*4 }

// Step advances one simulated second and returns this store's flow demands.
func (g *TrafficGenerator) Step(now time.Time) []FlowDemand {
	g.tick++
	hour := float64(now.Hour()) + float64(now.Minute())/60
	d := diurnal(hour)

	// 夜间：大流量（备份/更新）概率提高
	g.maybeSpawn(model.AppPOS, 0.05*d, now)
	g.maybeSpawn(model.AppVC, 0.03*d, now)
	g.maybeSpawn(model.AppWeb, 0.10*d, now)
	g.maybeSpawn(model.AppGeneral, 0.03*d, now)
	if hour >= 22 || hour < 6 {
		g.maybeSpawn(model.AppBulk, 0.06, now)
	} else {
		g.maybeSpawn(model.AppBulk, 0.012*d, now)
	}

	out := make([]FlowDemand, 0, len(g.active))
	kept := g.active[:0]
	for _, fg := range g.active {
		fg.age++
		dem := FlowDemand{
			Key: fg.key, App: fg.app, Dst: fg.dst,
			PktCount: fg.pktCount(now), MeanPkt: fg.pktSize,
			IntervalS: fg.interval,
		}
		// 速率带随机波动
		jitter := 0.7 + 0.6*g.rng.Float64()
		dem.Bytes = int64(fg.bps / 8 * jitter)
		if dem.Bytes < 1 {
			dem.Bytes = 1
		}
		if fg.first {
			dem.First = true
			dem.Payload = fg.payload
			fg.first = false
		}
		if fg.age >= fg.durTicks {
			dem.Ending = true
		}
		out = append(out, dem)
		if !dem.Ending {
			kept = append(kept, fg)
		}
	}
	g.active = kept
	return out
}

func (fg *flowGen) pktCount(now time.Time) int {
	if fg.interval > 0 {
		return int(1 / fg.interval)
	}
	if fg.pktSize <= 0 {
		return 1
	}
	return int(fg.bps/8)/fg.pktSize + 1
}

func (g *TrafficGenerator) maybeSpawn(app model.AppID, p float64, now time.Time) {
	if g.rng.Float64() >= p*g.loadMul {
		return
	}
	// 同应用并发流上限，避免无限增长
	n := 0
	for _, fg := range g.active {
		if fg.app == app {
			n++
		}
	}
	if n >= maxConcurrent(app) {
		return
	}
	g.active = append(g.active, g.spawn(app, now))
}

func maxConcurrent(app model.AppID) int {
	switch app {
	case model.AppPOS:
		return 3
	case model.AppVC:
		return 2
	case model.AppBulk:
		return 2
	case model.AppWeb:
		return 6
	default:
		return 3
	}
}

func (g *TrafficGenerator) spawn(app model.AppID, now time.Time) *flowGen {
	proto := "tcp"
	pktSize := 800
	interval := 0.0
	var bps float64
	dur := 60 + g.rng.Intn(240)

	switch app {
	case model.AppPOS:
		// 交易：小包、稀疏突发、短连接
		proto, pktSize = "tcp", 120+g.rng.Intn(120)
		bps = 2_000 + g.rng.Float64()*8_000
		dur = 20 + g.rng.Intn(100)
	case model.AppVC:
		// 会议：UDP，20ms 周期小包，持续几分钟
		proto, pktSize, interval = "udp", 160+g.rng.Intn(80), 0.02
		bps = 300_000 + g.rng.Float64()*900_000
		dur = 120 + g.rng.Intn(480)
	case model.AppBulk:
		// 备份/更新：大包、长连接、高带宽
		pktSize = 1200
		bps = 1_000_000 + g.rng.Float64()*5_000_000
		dur = 600 + g.rng.Intn(3000)
	case model.AppWeb:
		pktSize = 600 + g.rng.Intn(500)
		bps = 20_000 + g.rng.Float64()*150_000
		dur = 30 + g.rng.Intn(200)
	case model.AppGeneral:
		// 未知服务：刻意不匹配任何规则/签名，由行为归类兜底
		pktSize = 200 + g.rng.Intn(900)
		bps = 10_000 + g.rng.Float64()*200_000
		dur = 60 + g.rng.Intn(300)
	}

	dst := g.pickDst(app)
	host := g.store.Info.Subnet + fmt.Sprintf("%d", 10+g.rng.Intn(40))
	key := model.FlowKey{
		Proto: proto,
		Src:   model.Endpoint{IP: host, Port: uint16(30000 + g.rng.Intn(30000))},
		Dst:   dst,
	}
	fg := &flowGen{
		key: key, app: app, dst: dst, durTicks: dur, bps: bps,
		pktSize: pktSize, interval: interval,
		rng: rand.New(rand.NewSource(g.rng.Int63())), first: true,
		payload: g.firstPayload(app, dst),
	}
	return fg
}

func (g *TrafficGenerator) pickDst(app model.AppID) model.Endpoint {
	// 未知流量指向不匹配任何规则的地址段
	if app == model.AppGeneral {
		return model.Endpoint{
			IP:   fmt.Sprintf("198.51.100.%d", 20+g.rng.Intn(60)),
			Port: uint16(6000 + g.rng.Intn(4000)),
		}
	}
	// 其余走拓扑服务器表（由 Fluid 引擎统一持有，这里用内联表保持生成器独立）
	return knownDst(app, g.rng)
}

func (g *TrafficGenerator) firstPayload(app model.AppID, dst model.Endpoint) []byte {
	switch app {
	case model.AppPOS:
		// 部分 POS 流靠首包 ISO8583 签名识别（其余靠端口/DNS）
		if g.rng.Float64() < 0.5 {
			return []byte("ISO8583-0200-pay.example.com")
		}
	case model.AppVC:
		if g.rng.Float64() < 0.4 {
			// RTP 版本字节 0x80
			return append([]byte{0x80, 0x60, 0x00, 0x01}, []byte("meet")...)
		}
	case model.AppBulk:
		if g.rng.Float64() < 0.3 {
			return []byte("@RSYNCBCD")
		}
	}
	// 普通 TLS 头，无 marker，逼迫规则/DNS 路径
	return []byte{0x16, 0x03, 0x03, 0x01, 0x10}
}
