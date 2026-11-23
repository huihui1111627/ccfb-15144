package edge

import (
	"time"

	"sdwan/internal/classify"
	"sdwan/internal/model"
)

// NewFlow is the ingress request for a flow that has not yet been seen.
type NewFlow struct {
	Key     model.FlowKey
	First   bool
	Payload []byte
	Truth   model.AppID // 仿真环境中的真实应用（验收分类准确率用）
	RateBps float64     // 流体引擎给出的当前需求速率
}

// AdmitFlow classifies a new flow, caches the verdict for its lifetime and
// selects its initial link. Both simulation engines call this on flow start.
func (e *Engine) AdmitFlow(in NewFlow) *model.FlowState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f, ok := e.flows[in.Key]; ok {
		f.LastSeen = e.now
		return f
	}
	app, why := e.cls.Classify(classify.Info{Key: in.Key, First: in.First, Payload: in.Payload}, e.now)
	f := &model.FlowState{
		Key: in.Key, App: app, ClassWhy: why,
		StartedAt: e.now, LastSeen: e.now, Since: e.now,
		Active: true, Truth: in.Truth,
	}
	d := e.chooseLink(f)
	if d.Primary != "" {
		f.Primary = d.Primary
		f.Replica = d.Replica
	}
	e.flows[in.Key] = f
	e.pushEvent(model.FlowEvent{
		Flow: in.Key, App: app, To: d.Primary, Replica: d.Replica,
		Reason: "admit:" + d.Reason + "/" + why,
	})
	return f
}

// PromoteByBehavior applies a behavioral classification once enough evidence
// exists; the flow is re-routed because its SLA class may change.
func (e *Engine) PromoteByBehavior(k model.FlowKey, obs classify.BehaviorObs) {
	app, promoted := e.cls.ObserveBehavior(k, obs, e.now)
	if !promoted {
		return
	}
	e.mu.Lock()
	f, ok := e.flows[k]
	if !ok {
		e.mu.Unlock()
		return
	}
	old := f.App
	f.App = app
	if cached, hit := e.cls.Cached()[k]; hit {
		f.ClassWhy = cached.ClassWhy
	}
	d := e.chooseLink(f)
	if old != app && d.Primary != f.Primary {
		e.migrate(f, d.Primary, "reclassify:"+string(old)+"->"+string(app))
	}
	e.mu.Unlock()
}

// TickInput is one flow's demand for a simulated second (fluid engine), or the
// observed demand in packet mode.
type TickInput struct {
	Key     model.FlowKey
	Demand  int64 // 本 tick 想发的字节数
	RateBps float64
	Truth   model.AppID // 生成器真值（仅仿真用，用于分类准确率）
}

// TickOutput is the per-flow forwarding plan for the tick.
type TickOutput struct {
	Key     model.FlowKey
	App     model.AppID
	Truth   model.AppID
	Primary model.LinkType
	Replica model.LinkType
	// Sent carries bytes per link, indexed by model.LinkIndex. The fixed
	// array avoids a map allocation per flow per tick in large simulations.
	Sent      [model.NumLinkTypes]int64
	Offered   int64 // 本 tick 需求字节（含被限速部分）
	Throttled int64 // 被限速器压掉的字节
	CongLost  int64 // 链路容量不足导致的丢失字节
	SLA       bool  // 本 tick 是否达标
	Why       string
}

// flowPair couples a flow state with its tick demand for priority sorting.
type flowPair struct {
	f  *model.FlowState
	ti TickInput
}

// Tick advances one simulated second: probes, policy re-evaluation with
// hysteresis, bulk shaping and per-flow allocation. Inputs are this edge's
// active flow demands; outputs are the concrete per-link byte plan that the
// transport layer applies to the simulated links.
func (e *Engine) Tick(now time.Time, in []TickInput) []TickOutput {
	e.mu.Lock()
	e.now = now
	// 1) 探测：流体模式下对每条链路做一次合成探测
	for _, t := range e.order {
		e.trackers[t].SyntheticTick(now)
		e.trackers[t].TimeoutPending(now, 3*e.policy.ProbeInterval)
	}
	// 2) 限速器按秒补充令牌
	e.shaper.refill()

	// 优先级排序：关键/高优先级流量先拿链路资源（一次配对，无 map 分配）
	pairs := make([]flowPair, 0, len(in))
	for _, ti := range in {
		if f, ok := e.flows[ti.Key]; ok {
			pairs = append(pairs, flowPair{f: f, ti: ti})
		}
	}
	sortPairs(pairs, e.policy)

	out := make([]TickOutput, 0, len(pairs))
	var capUsed [model.NumLinkTypes]int64 // 本 tick 各链路已分配字节
	for _, p := range pairs {
		f, ti := p.f, p.ti
		f.LastSeen = now

		// 3) 重评估（滞回迁移 / 复制启停）
		d := e.chooseLink(f)
		migrated := e.reevaluate(f, d)
		if migrated {
			prof := e.policy.Profiles[f.App]
			if prof == nil || !prof.Realtime {
				e.reorder.Arm(f.Key, now)
			}
		}
		f.Migrating = false

		plan := TickOutput{
			Key: f.Key, App: f.App, Truth: ti.Truth,
			Primary: f.Primary, Replica: f.Replica, Offered: ti.Demand,
		}

		// 4) 大流量限速
		demand := ti.Demand
		prof := e.policy.Profiles[f.App]
		if prof != nil && prof.Bulk {
			allowed := e.shaper.take(demand)
			plan.Throttled = demand - allowed
			demand = allowed
		}
		if demand <= 0 {
			// 主动限速是预期行为：大流量本就允许被整形，不计为 SLA 违约
			plan.Why = "shaped"
			if prof != nil && prof.Bulk {
				plan.SLA = true
			}
			out = append(out, plan)
			continue
		}

		// 5) 按链路容量分配（主链路；复制时在副本上发同样的字节）
		avail := e.linkAvailable(f.Primary, capUsed, now)
		sent := demand
		if sent > avail {
			sent = avail
		}
		capUsed[model.LinkIndex(f.Primary)] += sent
		plan.Sent[model.LinkIndex(f.Primary)] = sent
		e.addBytes(f.Primary, sent)
		// 容量装不下的部分视为拥塞丢失/排队到下一个 tick
		congLost := demand - sent

		if f.Replica != "" {
			avail2 := e.linkAvailable(f.Replica, capUsed, now)
			dup := demand
			if dup > avail2 {
				dup = avail2
			}
			capUsed[model.LinkIndex(f.Replica)] += dup
			plan.Sent[model.LinkIndex(f.Replica)] = dup
			e.addBytes(f.Replica, dup)
			// 双发时只要有一条链路完整承载，应用层就不丢
			if dup >= demand-congLost {
				congLost = 0
			} else if dup < demand {
				congLost = demand - maxInt64(sent, dup)
			}
		}

		// 6) 利用率回灌探测器
		for _, t := range e.order {
			m := e.trackers[t].Link()
			util := float64(capUsed[model.LinkIndex(t)]) / (m.Config().UpBps / 8)
			e.trackers[t].SetUtil(util)
		}

		// 7) SLA 判定：链路实测质量 + 本 tick 是否完整交付
		q := e.trackers[f.Primary].Quality()
		plan.SLA = meetsSLA(prof, q) && congLost == 0
		plan.CongLost = congLost
		switch {
		case !plan.SLA && congLost > 0:
			plan.Why = "sla-violation-capacity"
		case plan.SLA && f.Replica != "":
			plan.Why = "replicated-ok"
		case plan.SLA:
			plan.Why = "ok"
		default:
			plan.Why = "sla-violation"
		}
		out = append(out, plan)
	}
	e.mu.Unlock()
	return out
}

// linkAvailable returns unused uplink bytes on a link for this tick.
func (e *Engine) linkAvailable(t model.LinkType, used [model.NumLinkTypes]int64, now time.Time) int64 {
	tr, ok := e.trackers[t]
	if !ok {
		return 0
	}
	q := tr.Quality()
	if !q.Healthy {
		return 0
	}
	capBytes := int64(q.UpBps / 8)
	remain := capBytes - used[model.LinkIndex(t)]
	if remain < 0 {
		return 0
	}
	return remain
}

func meetsSLA(prof *model.AppProfile, q model.Quality) bool {
	if prof == nil {
		return q.Healthy
	}
	if !q.Healthy {
		return false
	}
	return q.RTT <= prof.MaxRTT && q.Loss <= prof.MaxLoss && q.Jitter <= prof.MaxJitter
}

// sortPairs orders flows by application priority (highest first) so critical
// traffic claims link capacity before background traffic.
func sortPairs(pairs []flowPair, p *model.Policy) {
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0; j-- {
			if prio(pairs[j].f, p) > prio(pairs[j-1].f, p) {
				pairs[j], pairs[j-1] = pairs[j-1], pairs[j]
			} else {
				break
			}
		}
	}
}

func prio(f *model.FlowState, p *model.Policy) int {
	if pr := p.Profiles[f.App]; pr != nil {
		return pr.Priority
	}
	return 0
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
