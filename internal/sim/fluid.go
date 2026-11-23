package sim

import (
	"runtime"
	"sync"
	"time"

	"sdwan/internal/classify"
	"sdwan/internal/controller"
	"sdwan/internal/edge"
	"sdwan/internal/model"
	"sdwan/internal/network"
)

// FluidConfig configures an acceptance run.
type FluidConfig struct {
	Stores    int
	Duration  time.Duration
	Seed      int64
	LoadMul   float64
	SyncEvery time.Duration // 边缘与控制器同步周期
	ReapEvery time.Duration
	Workers   int // 并行 worker 数；0 = GOMAXPROCS
}

// edgeRuntime bundles one store's per-edge simulation state.
type edgeRuntime struct {
	store  *network.Store
	gen    *TrafficGenerator
	eng    *edge.Engine
	client *controller.InProcessClient
	agent  *controller.Agent
	flows  map[model.FlowKey]*liveFlow
	bps    map[model.FlowKey]float64
	acc    *LocalAcc
	inBuf  []edge.TickInput // 跨 tick 复用，避免每 tick 重新分配
}

type liveFlow struct {
	demand FlowDemand
	app    model.AppID
}

// Fluid is the fluid (aggregate, 1-second quanta) simulation engine.
type Fluid struct {
	cfg      FluidConfig
	tp       *network.Topology
	ctrl     *controller.Controller
	scen     *Scenario
	met      *Metrics
	runtimes []*edgeRuntime
	fr       *faultRuntime
	t0       time.Time
	now      time.Time
	step     int64
	workers  int
	pool     *pool
	// tickResults is the per-tick output array read by persistent pool workers.
	tickResults []edgeResult
	// 每分钟一个时间序列桶
	lastBucket time.Time
	bucket     *SeriesBucket
}

// SeriesBucket accumulates per-minute data for the dashboard/report charts.
type SeriesBucket struct {
	At       time.Time
	mplsBps  float64
	bbBps    float64
	cellBps  float64
	slaMet   map[model.AppID]int64
	slaTot   map[model.AppID]int64
	critDown int
	samples  int
}

// NewFluid builds a full simulation for the given mode.
func NewFluid(mode edge.Mode, cfg FluidConfig) *Fluid {
	t0 := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC) // 周一 00:00 起
	tp := network.BuildTopology(cfg.Stores, cfg.Seed)
	policy := model.DefaultPolicy()
	ctrl := controller.New(policy)
	scen := DefaultScenario(t0, cfg.Stores)

	f := &Fluid{
		cfg: cfg, tp: tp, ctrl: ctrl, scen: scen, met: NewMetrics(),
		fr: newFaultRuntime(scen), t0: t0, now: t0,
		workers: cfg.Workers,
	}
	if f.workers <= 0 {
		f.workers = runtime.GOMAXPROCS(0)
	}
	gc := GeneratorConfig{LoadMul: cfg.LoadMul}
	for i, st := range tp.Stores {
		hub := tp.HubByRegion[st.Info.Region]
		var links []*network.LinkModel
		for _, t := range st.Info.Links {
			links = append(links, st.Links[t])
		}
		eng := edge.NewEngine(st.Info.ID, mode, model.DefaultPolicy(), links)
		cl := controller.NewInProcessClient(ctrl, model.DefaultPolicy())
		ag := controller.NewAgent(st.Info.ID, cl, eng.UpdatePolicy, eng.SetControllerUp)
		rt := &edgeRuntime{
			store:  st,
			gen:    NewGenerator(st, hub, cfg.Seed+int64(i)*101+7, gc),
			eng:    eng,
			client: cl,
			agent:  ag,
			flows:  map[model.FlowKey]*liveFlow{},
			bps:    map[model.FlowKey]float64{},
			acc:    NewLocalAcc(st.Info.ID),
		}
		f.runtimes = append(f.runtimes, rt)
	}
	f.tickResults = make([]edgeResult, len(f.runtimes))
	if f.workers > 1 && len(f.runtimes) > 1 {
		f.pool = newPool(f.workers, func(i int) {
			f.tickResults[i] = f.computeEdge(f.runtimes[i])
		})
	}
	return f
}

// Topology exposes the network (for the web/driver).
func (f *Fluid) Topology() *network.Topology { return f.tp }

// Controller exposes the central controller.
func (f *Fluid) Controller() *controller.Controller { return f.ctrl }

// Metrics exposes the aggregator.
func (f *Fluid) Metrics() *Metrics { return f.met }

// Scenario exposes the fault plan.
func (f *Fluid) Scenario() *Scenario { return f.scen }

// Now returns simulated time.
func (f *Fluid) Now() time.Time { return f.now }

// Run executes the whole simulation and returns final metrics.
func (f *Fluid) Run() *Metrics {
	ticks := int(f.cfg.Duration / time.Second)
	for t := 0; t < ticks; t++ {
		f.Step()
	}
	// 关闭各边缘仍打开的中断窗口并汇总
	for _, rt := range f.runtimes {
		for _, in := range rt.acc.CloseAll(f.now) {
			f.met.recordInterruption(in)
		}
	}
	f.met.CloseInterruptions(f.now)
	return f.met
}

// edgeResult holds one edge's computed tick for the serial merge phase.
type edgeResult struct {
	rt         *edgeRuntime
	outputs    []edge.TickOutput
	autonomous bool
	events     []model.FlowEvent
}

// pool is a set of long-lived worker goroutines. Each tick the driver sends
// every edge index into jobs (channel stays open) and waits on a fresh
// WaitGroup, so no goroutine or channel is allocated per tick.
type pool struct {
	jobs chan poolJob
	n    int
}

type poolJob struct {
	idx int
	wg  *sync.WaitGroup
}

// newPool starts n persistent workers running work.
func newPool(n int, work func(int)) *pool {
	if n < 1 {
		n = 1
	}
	p := &pool{jobs: make(chan poolJob, n), n: n}
	for w := 0; w < n; w++ {
		go func() {
			for j := range p.jobs {
				work(j.idx)
				j.wg.Done()
			}
		}()
	}
	return p
}

// run submits nJobs indices and blocks until all complete this tick.
func (p *pool) run(nJobs int, wg *sync.WaitGroup) {
	for i := 0; i < nJobs; i++ {
		p.jobs <- poolJob{idx: i, wg: wg}
	}
	wg.Wait()
}

// Step advances one simulated second. Edge computations are independent and
// run across a persistent worker pool; only the merge into shared metrics is
// serial.
func (f *Fluid) Step() {
	f.now = f.t0.Add(time.Duration(f.step) * time.Second)
	f.step++
	// 故障状态推进（在所有边缘计算之前，串行且廉价）
	f.fr.Apply(f.now, f.tp, func(down bool) {
		for _, rt := range f.runtimes {
			f.ctrl.SetLinkDown(rt.store.Info.ID, down)
		}
	})

	if f.pool == nil {
		for i := range f.runtimes {
			f.tickResults[i] = f.computeEdge(f.runtimes[i])
		}
	} else {
		var wg sync.WaitGroup
		wg.Add(len(f.runtimes))
		f.pool.run(len(f.runtimes), &wg)
	}

	// 串行合并：指标、控制器遥测、时间序列桶
	for i := range f.runtimes {
		f.mergeEdge(f.tickResults[i])
	}
	f.fillBucket()
}

// computeEdge runs all per-edge, contention-free work for the tick.
func (f *Fluid) computeEdge(rt *edgeRuntime) edgeResult {
	rt.eng.SetNow(f.now) // 让分类/接纳时间戳与当前 tick 对齐
	rt.acc.Reset()
	demands := rt.gen.Step(f.now)
	rt.inBuf = rt.inBuf[:0]
	inputs := rt.inBuf
	res := edgeResult{rt: rt}

	for _, d := range demands {
		// 行为观察：只为仍处于 pending（未被规则/签名识别）的流积累证据
		if d.PktCount > 0 {
			if existing, ok := rt.eng.Flow(d.Key); ok && existing.ClassWhy == "pending" {
				rt.eng.PromoteByBehavior(d.Key, classify.BehaviorObs{
					Packets: d.PktCount, Bytes: d.Bytes,
					MeanPkt: float64(d.MeanPkt), IntervalS: d.IntervalS,
				})
			}
		}
		rt.eng.AdmitFlow(edge.NewFlow{
			Key: d.Key, First: d.First, Payload: d.Payload,
			Truth: d.App, RateBps: float64(d.Bytes) * 8,
		})
		rt.flows[d.Key] = &liveFlow{demand: d, app: d.App}
		rt.bps[d.Key] = float64(d.Bytes) * 8
		inputs = append(inputs, edge.TickInput{
			Key: d.Key, Demand: d.Bytes, RateBps: float64(d.Bytes) * 8, Truth: d.App,
		})
		if d.Ending {
			rt.eng.EndFlow(d.Key)
			rt.acc.AddFlowEnd(f.now, d.Key)
			delete(rt.flows, d.Key)
			delete(rt.bps, d.Key)
		}
	}

	res.outputs = rt.eng.Tick(f.now, inputs)
	for _, o := range res.outputs {
		rt.acc.AddOutput(f.now, o)
	}

	// 控制器同步（周期性；失联时 Agent 内部自动用最后策略）
	if f.cfg.SyncEvery <= 0 || f.now.Sub(f.t0)%f.cfg.SyncEvery == 0 {
		snap := rt.eng.Snapshot(rt.bps)
		up := rt.agent.Sync(snap)
		res.autonomous = !up
	} else {
		res.autonomous = !rt.eng.ControllerUp()
	}
	if res.autonomous {
		rt.acc.MarkAutonomous()
	}
	res.events = rt.eng.DrainEvents()
	rt.acc.AddEvents(res.events)
	return res
}

// mergeEdge folds one computed edge result into shared simulation state.
func (f *Fluid) mergeEdge(r edgeResult) {
	f.met.Merge(r.rt.acc)
	f.accumulate(r.outputs)
}

func (f *Fluid) fillBucket() {
	if f.lastBucket.IsZero() {
		f.bucket = &SeriesBucket{At: f.now, slaMet: map[model.AppID]int64{}, slaTot: map[model.AppID]int64{}}
		f.lastBucket = f.now
	}
	if f.now.Sub(f.lastBucket) >= time.Minute {
		b := f.bucket
		sp := SeriesPoint{At: b.At, MPLSBps: b.mplsBps / 60, BBBps: b.bbBps / 60,
			CellBps: b.cellBps / 60, SLARate: map[model.AppID]float64{}, CriticalDown: b.critDown}
		for _, a := range model.AllApps {
			if b.slaTot[a] > 0 {
				sp.SLARate[a] = float64(b.slaMet[a]) / float64(b.slaTot[a])
			}
		}
		f.met.addSeries(sp)
		f.bucket = &SeriesBucket{At: f.now, slaMet: map[model.AppID]int64{}, slaTot: map[model.AppID]int64{}}
		f.lastBucket = f.now
	}
}

func (f *Fluid) accumulate(outs []edge.TickOutput) {
	if f.bucket == nil {
		return
	}
	b := f.bucket
	for _, o := range outs {
		b.mplsBps += float64(o.Sent[model.LinkIndex(model.LinkMPLS)]) * 8
		b.bbBps += float64(o.Sent[model.LinkIndex(model.LinkBroadband)]) * 8
		b.cellBps += float64(o.Sent[model.LinkIndex(model.LinkCellular)]) * 8
		b.slaTot[o.App]++
		if o.SLA {
			b.slaMet[o.App]++
		} else if model.DefaultProfiles()[o.App].Critical {
			b.critDown++
		}
	}
}
