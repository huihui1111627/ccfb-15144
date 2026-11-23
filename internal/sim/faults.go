package sim

import (
	"fmt"
	"sort"
	"time"

	"sdwan/internal/model"
	"sdwan/internal/network"
)

// FaultKind enumerates injected events.
type FaultKind string

const (
	FaultMPLSCongestion   FaultKind = "mpls-congestion" // 专线拥塞：压缩容量
	FaultBroadbandDown    FaultKind = "broadband-down"  // 宽带完全中断
	FaultBroadbandDegrade FaultKind = "broadband-degrade"
	FaultCellularDown     FaultKind = "cellular-down"
	FaultControllerDown   FaultKind = "controller-down" // 控制器失联
)

// Fault is one scheduled event.
type Fault struct {
	Kind     FaultKind
	Start    time.Time
	Duration time.Duration
	// Targets: store id prefixes/ids; empty = all stores. Controller faults
	// use the controller's edge group.
	Targets []string
	// Congestion capacity multiplier (e.g. 0.25 keeps 25%).
	CapMult float64
}

// Scenario is the full deterministic event plan of a run.
type Scenario struct {
	Faults []Fault
}

// DefaultScenario builds the acceptance scenario: recurring business-hour MPLS
// congestion, evening broadband degradation, and several hard outages spread
// over the run, including a controller isolation window.
func DefaultScenario(t0 time.Time, nStores int) *Scenario {
	s := &Scenario{}
	day := 24 * time.Hour
	// 每天营业高峰注入一次专线拥塞（持续 2 小时）
	for d := 0; d < 3; d++ {
		s.Faults = append(s.Faults, Fault{
			Kind:     FaultMPLSCongestion,
			Start:    t0.Add(time.Duration(d)*day + 10*time.Hour),
			Duration: 2 * time.Hour, CapMult: 0.2,
		})
	}
	// 第 1 天下午部分门店宽带故障 90 分钟
	s.Faults = append(s.Faults, Fault{
		Kind:  FaultBroadbandDown,
		Start: t0.Add(day + 14*time.Hour), Duration: 90 * time.Minute,
		Targets: pickStores(nStores, 0.1, 11),
	})
	// 第 2 天晚上宽带整体劣化叠加局部中断
	s.Faults = append(s.Faults, Fault{
		Kind:  FaultBroadbandDegrade,
		Start: t0.Add(2*day + 20*time.Hour), Duration: 3 * time.Hour,
		CapMult: 0.35,
	})
	s.Faults = append(s.Faults, Fault{
		Kind:     FaultBroadbandDown,
		Start:    t0.Add(2*day + 20*time.Hour + 30*time.Minute),
		Duration: 2 * time.Hour,
		Targets:  pickStores(nStores, 0.05, 23),
	})
	// 第 3 天移动网卡局部故障
	s.Faults = append(s.Faults, Fault{
		Kind:  FaultCellularDown,
		Start: t0.Add(3*day - 6*time.Hour), Duration: 2 * time.Hour,
		Targets: pickStores(nStores, 0.2, 37),
	})
	// 控制器失联 1 小时（边缘按最后策略自治）
	s.Faults = append(s.Faults, Fault{
		Kind:  FaultControllerDown,
		Start: t0.Add(2*day + 11*time.Hour), Duration: time.Hour,
	})
	return s
}

func pickStores(n int, frac float64, seed int) []string {
	// 确定性选取，不依赖全局随机源
	count := int(float64(n) * frac)
	if count < 1 && n > 0 {
		count = 1
	}
	step := n / count
	if step < 1 {
		step = 1
	}
	out := make([]string, 0, count)
	for i := seed % max1(step); len(out) < count; i += step {
		out = append(out, storeID(i%n+1))
	}
	sort.Strings(out)
	return out
}

func max1(x int) int {
	if x < 1 {
		return 1
	}
	return x
}

func storeID(i int) string {
	return "store-" + pad4(i)
}

func pad4(i int) string {
	if i >= 1000 {
		return itoaN(i, 4)
	}
	if i >= 100 {
		return "0" + itoaN(i, 3)
	}
	if i >= 10 {
		return "00" + itoaN(i, 2)
	}
	return "000" + itoaN(i, 1)
}

func itoaN(i, n int) string {
	b := make([]byte, n)
	for k := n - 1; k >= 0; k-- {
		b[k] = byte('0' + i%10)
		i /= 10
	}
	return string(b)
}

// faultRuntime tracks active faults and applies them to the topology.
type faultRuntime struct {
	scenario *Scenario
	active   map[int]*Fault
	// 保存被压缩的原始容量
	savedCap map[model.LinkID][2]float64
}

func newFaultRuntime(s *Scenario) *faultRuntime {
	return &faultRuntime{scenario: s, active: map[int]*Fault{}, savedCap: map[model.LinkID][2]float64{}}
}

// Apply updates outage/congestion state at virtual time now. It returns the
// list of faults whose state changed this tick.
func (fr *faultRuntime) Apply(now time.Time, tp *network.Topology, ctrlDown func(bool)) []Fault {
	changed := []Fault{}
	for i, f := range fr.scenario.Faults {
		end := f.Start.Add(f.Duration)
		activeNow := !now.Before(f.Start) && now.Before(end)
		_, wasActive := fr.active[i]
		if activeNow == wasActive {
			continue
		}
		if activeNow {
			fr.active[i] = &fr.scenario.Faults[i]
		} else {
			delete(fr.active, i)
		}
		changed = append(changed, f)
		fr.applyOne(f, activeNow, tp, ctrlDown)
	}
	return changed
}

func (fr *faultRuntime) applyOne(f Fault, on bool, tp *network.Topology, ctrlDown func(bool)) {
	switch f.Kind {
	case FaultControllerDown:
		ctrlDown(on)
		return
	case FaultMPLSCongestion:
		for _, st := range tp.Stores {
			if !targetMatch(st.Info.ID, f.Targets) {
				continue
			}
			fr.setCap(st.Links[model.LinkMPLS], on, f.CapMult)
		}
	case FaultBroadbandDown, FaultCellularDown:
		var t model.LinkType
		if f.Kind == FaultBroadbandDown {
			t = model.LinkBroadband
		} else {
			t = model.LinkCellular
		}
		for _, st := range tp.Stores {
			if !targetMatch(st.Info.ID, f.Targets) {
				continue
			}
			if l, ok := st.Links[t]; ok {
				l.SetOutage(on)
			}
		}
	case FaultBroadbandDegrade:
		for _, st := range tp.Stores {
			if !targetMatch(st.Info.ID, f.Targets) {
				continue
			}
			fr.setCap(st.Links[model.LinkBroadband], on, f.CapMult)
		}
	}
}

func (fr *faultRuntime) setCap(l *network.LinkModel, on bool, mult float64) {
	if l == nil {
		return
	}
	id := l.ID()
	if on {
		cfg := l.Config()
		fr.savedCap[id] = [2]float64{cfg.UpBps, cfg.DownBps}
		cfg.UpBps *= mult
		cfg.DownBps *= mult
		l.SetConfig(cfg)
	} else if saved, ok := fr.savedCap[id]; ok {
		cfg := l.Config()
		cfg.UpBps, cfg.DownBps = saved[0], saved[1]
		l.SetConfig(cfg)
		delete(fr.savedCap, id)
	}
}

func targetMatch(id string, targets []string) bool {
	if len(targets) == 0 {
		return true
	}
	for _, t := range targets {
		if t == id {
			return true
		}
	}
	return false
}

// FaultSummary renders the scenario's events as human-readable lines, keeping
// only faults that fall at least partly inside the simulated window.
func (s *Scenario) FaultSummary(t0 time.Time, dur time.Duration) []string {
	end := t0.Add(dur)
	var lines []string
	for _, f := range s.Faults {
		if !f.Start.Before(end) {
			continue
		}
		day := int(f.Start.Sub(t0).Hours()/24) + 1
		clock := f.Start.Format("15:04")
		scope := "全网门店"
		if n := len(f.Targets); n > 0 {
			scope = fmt.Sprintf("%d 家指定门店", n)
		}
		var name string
		switch f.Kind {
		case FaultMPLSCongestion:
			name = fmt.Sprintf("专线拥塞：容量压缩至 %.0f%%", f.CapMult*100)
		case FaultBroadbandDown:
			name = "宽带完全中断"
		case FaultBroadbandDegrade:
			name = fmt.Sprintf("宽带晚高峰劣化：容量降至 %.0f%%", f.CapMult*100)
		case FaultCellularDown:
			name = "移动网卡故障"
		case FaultControllerDown:
			name = "中心控制器失联（验证边缘自治）"
			scope = "全网"
		}
		lines = append(lines, fmt.Sprintf("第 %d 天 %s，%s，%s，持续 %s",
			day, clock, scope, name, f.Duration.Round(time.Second)))
	}
	if len(lines) == 0 {
		lines = append(lines, "本窗口内无注入故障")
	}
	return lines
}
