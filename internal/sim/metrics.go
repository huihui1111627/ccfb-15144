package sim

import (
	"sync"
	"time"

	"sdwan/internal/model"
)

// AppMetrics accumulates one application's acceptance statistics.
type AppMetrics struct {
	App            model.AppID
	Samples        int64 // SLA 采样次数（每流每秒）
	SLAMet         int64
	InterruptedS   int64 // 质量不达标的秒数
	BytesOffered   int64
	BytesDelivered int64
	Throttled      int64
}

// SLA rate in 0..1.
func (m *AppMetrics) SLARate() float64 {
	if m.Samples == 0 {
		return 0
	}
	return float64(m.SLAMet) / float64(m.Samples)
}

// LinkMetrics accumulates per-link-type usage across the whole network.
type LinkMetrics struct {
	LinkType   model.LinkType
	BytesUp    int64
	Replicated int64 // 复制冗余字节（接收端去重前）
	Seconds    int64
}

// Interruption tracks one continuous critical-app outage window.
type Interruption struct {
	EdgeID string
	App    model.AppID
	Flow   model.FlowKey
	Start  time.Time
	End    time.Time
}

func (i Interruption) Duration() time.Duration { return i.End.Sub(i.Start) }

// Metrics is the global acceptance aggregator.
type Metrics struct {
	mu sync.Mutex

	App       map[model.AppID]*AppMetrics
	Link      map[model.LinkType]*LinkMetrics
	Interrupt []Interruption
	// 当前仍在中断中的关键流
	openInter map[interKey]*Interruption

	Migrations   int64
	Replications int64
	Flaps        int64 // 乒乓抖动：流在 flapWindow 内迁回刚离开的链路
	// 每条流最近一次所在链路及迁移时刻，用于乒乓判定
	lastMigrate map[flowMigKey]migrationRec

	// 分类准确率：识别结果 vs 生成器真值
	ClassSamples int64
	ClassCorrect int64
	ClassByWhy   map[string]int64

	// 控制器自治时长（秒）
	AutonomousS int64

	// 时间序列（每分钟一个桶，供页面/报告画图）
	Series []SeriesPoint

	ticks int64
}

type interKey struct {
	edge string
	flow model.FlowKey
}

// flowMigKey identifies a flow across edges for flap counting.
type flowMigKey struct {
	edge string
	flow model.FlowKey
}

// migrationRec remembers a flow's previous link and when it moved away.
type migrationRec struct {
	from model.LinkType
	to   model.LinkType
	at   time.Time
}

// SeriesPoint is one per-minute network-wide sample.
type SeriesPoint struct {
	At           time.Time
	MPLSBps      float64
	BBBps        float64
	CellBps      float64
	SLARate      map[model.AppID]float64
	CriticalDown int
}

func NewMetrics() *Metrics {
	m := &Metrics{
		App:         map[model.AppID]*AppMetrics{},
		Link:        map[model.LinkType]*LinkMetrics{},
		openInter:   map[interKey]*Interruption{},
		ClassByWhy:  map[string]int64{},
		lastMigrate: map[flowMigKey]migrationRec{},
	}
	for _, a := range model.AllApps {
		m.App[a] = &AppMetrics{App: a}
	}
	for _, t := range model.AllLinkTypes {
		m.Link[t] = &LinkMetrics{LinkType: t}
	}
	return m
}

// recordInterruption appends a finalized interruption window.
func (m *Metrics) recordInterruption(in Interruption) {
	m.mu.Lock()
	m.Interrupt = append(m.Interrupt, in)
	m.mu.Unlock()
}

// CloseInterruptions flushes any open interruption windows at run end.
func (m *Metrics) CloseInterruptions(end time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, open := range m.openInter {
		open.End = end
		m.Interrupt = append(m.Interrupt, *open)
		delete(m.openInter, k)
	}
}

// CellularCost computes total mobile traffic cost from policy pricing.
func (m *Metrics) CellularCost(costPerGB float64) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return float64(m.Link[model.LinkCellular].BytesUp) / 1e9 * costPerGB
}

// Snapshot returns a copy safe to read without the lock.
func (m *Metrics) Snapshot() *Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := NewMetrics()
	cp.Migrations, cp.Replications, cp.Flaps = m.Migrations, m.Replications, m.Flaps
	cp.ClassSamples, cp.ClassCorrect = m.ClassSamples, m.ClassCorrect
	cp.AutonomousS, cp.ticks = m.AutonomousS, m.ticks
	cp.Interrupt = append([]Interruption(nil), m.Interrupt...)
	cp.Series = append([]SeriesPoint(nil), m.Series...)
	for k, v := range m.ClassByWhy {
		cp.ClassByWhy[k] = v
	}
	for k, v := range m.App {
		x := *v
		cp.App[k] = &x
	}
	for k, v := range m.Link {
		x := *v
		cp.Link[k] = &x
	}
	return cp
}

// ClassRate reports classification accuracy.
func (m *Metrics) ClassRate() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ClassSamples == 0 {
		return 0
	}
	return float64(m.ClassCorrect) / float64(m.ClassSamples)
}

// MeanCriticalInterrupt returns average interruption duration of critical apps.
func (m *Metrics) MeanCriticalInterrupt() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total time.Duration
	n := 0
	for _, in := range m.Interrupt {
		total += in.Duration()
		n++
	}
	if n == 0 {
		return 0
	}
	return total / time.Duration(n)
}

// MaxCriticalInterrupt returns the longest interruption.
func (m *Metrics) MaxCriticalInterrupt() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var maxD time.Duration
	for _, in := range m.Interrupt {
		if d := in.Duration(); d > maxD {
			maxD = d
		}
	}
	return maxD
}

// tickCount reports the number of edge-ticks observed (used by the fluid
// engine to derive simulated time deterministically).
func (m *Metrics) tickCount() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ticks
}

// addSeries appends a per-minute time-series point.
func (m *Metrics) addSeries(p SeriesPoint) {
	m.mu.Lock()
	m.Series = append(m.Series, p)
	m.mu.Unlock()
}

// SeriesCopy returns the time series.
func (m *Metrics) SeriesCopy() []SeriesPoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SeriesPoint(nil), m.Series...)
}

// TickCount reports total edge-ticks.
func (m *Metrics) TickCount() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ticks
}

// CompactMetrics is the space-efficient JSON view of a Metrics instance:
// per-interruption flow detail is replaced by aggregate statistics so the
// acceptance artifact stays small for hundreds of stores.
type CompactMetrics struct {
	App             map[model.AppID]*AppMetrics     `json:"app"`
	Link            map[model.LinkType]*LinkMetrics `json:"link"`
	InterruptCount  int                             `json:"interrupt_count"`
	InterruptMeanMs int64                           `json:"interrupt_mean_ms"`
	InterruptMaxMs  int64                           `json:"interrupt_max_ms"`
	Migrations      int64                           `json:"migrations"`
	Flaps           int64                           `json:"flaps"`
	Replications    int64                           `json:"replications"`
	ClassSamples    int64                           `json:"class_samples"`
	ClassCorrect    int64                           `json:"class_correct"`
	AutonomousTicks int64                           `json:"autonomous_ticks"`
	Ticks           int64                           `json:"edge_ticks"`
	Series          []SeriesPoint                   `json:"series,omitempty"`
}

// Compact returns a JSON-friendly aggregate view. Include series only when
// requested; it is the main size driver for long runs.
func (m *Metrics) Compact(includeSeries bool) CompactMetrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := CompactMetrics{
		App:            map[model.AppID]*AppMetrics{},
		Link:           map[model.LinkType]*LinkMetrics{},
		InterruptCount: len(m.Interrupt),
		Migrations:     m.Migrations, Flaps: m.Flaps, Replications: m.Replications,
		ClassSamples: m.ClassSamples, ClassCorrect: m.ClassCorrect,
		AutonomousTicks: m.AutonomousS, Ticks: m.ticks,
	}
	for k, v := range m.App {
		x := *v
		c.App[k] = &x
	}
	for k, v := range m.Link {
		x := *v
		c.Link[k] = &x
	}
	if len(m.Interrupt) > 0 {
		var sum time.Duration
		var maxD time.Duration
		for _, in := range m.Interrupt {
			d := in.Duration()
			sum += d
			if d > maxD {
				maxD = d
			}
		}
		c.InterruptMeanMs = (sum / time.Duration(len(m.Interrupt))).Milliseconds()
		c.InterruptMaxMs = maxD.Milliseconds()
	}
	if includeSeries {
		c.Series = append([]SeriesPoint(nil), m.Series...)
	}
	return c
}
