package sim

import (
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
)

// LocalAcc is one edge's lock-free per-tick accumulator. Each parallel worker
// writes its own instance; the serial merge folds these into the global Metrics
// under a single lock acquisition per edge per tick.
type LocalAcc struct {
	edgeID string

	appSamples [model.NumApps]int64
	appSLAMet  [model.NumApps]int64
	appThrot   [model.NumApps]int64
	appOffered [model.NumApps]int64
	appDeliv   [model.NumApps]int64
	appInter   [model.NumApps]int64

	linkBytes     [model.NumLinkTypes]int64
	linkReplicate [model.NumLinkTypes]int64
	linkSeconds   [model.NumLinkTypes]int64

	classSamples int64
	classCorrect int64
	replications int64
	autonomous   int64

	// 关键应用中断窗口（本 tick 新打开/待关闭）
	openInter map[interKey]*Interruption // 本地自持，跨 tick 保留
	opened    []Interruption
	closed    []model.FlowKey
	migrateN  int64
	events    []model.FlowEvent
	closeAt   time.Time
}

// appIndex maps an app id to a fixed index for the accumulator arrays.
func appIndex(a model.AppID) int {
	for i, x := range model.AllApps {
		if x == a {
			return i
		}
	}
	return 0
}

// flapWindow bounds ping-pong detection: a second migration of the same flow
// within this window counts as a flap. The edge's HoldDown (20s) already
// suppresses most of these.
const flapWindow = 60 * time.Second

// NewLocalAcc builds a per-edge accumulator.
func NewLocalAcc(edgeID string) *LocalAcc {
	return &LocalAcc{edgeID: edgeID, openInter: map[interKey]*Interruption{}}
}

// Reset clears the per-tick counters while preserving cross-tick interruption
// state.
func (l *LocalAcc) Reset() {
	open := l.openInter
	*l = LocalAcc{edgeID: l.edgeID, openInter: open}
}

// AddOutput folds one flow's tick output into the local counters.
func (l *LocalAcc) AddOutput(now time.Time, o edge.TickOutput) {
	ai := appIndex(o.App)
	l.appSamples[ai]++
	if o.SLA {
		l.appSLAMet[ai]++
	} else {
		l.appInter[ai]++
	}
	l.appThrot[ai] += o.Throttled
	l.appOffered[ai] += o.Offered

	// 链路用量含复制副本（真实占用带宽），逐链路上报
	var delivered int64
	for i, n := range o.Sent {
		if n == 0 {
			continue
		}
		l.linkBytes[i] += n
		l.linkSeconds[i]++
	}
	// 应用交付量只计主链路：复制副本在接收端去重，不会产生额外应用数据
	delivered = o.Sent[model.LinkIndex(o.Primary)]
	l.appDeliv[ai] += delivered

	if o.Truth != "" {
		l.classSamples++
		if o.Truth == o.App {
			l.classCorrect++
		}
	}
	if o.Replica != "" {
		l.replications++
		for i, n := range o.Sent {
			if n != 0 && model.LinkByIndex(i) != o.Primary {
				l.linkReplicate[i] += n
			}
		}
	}

	// 关键应用中断窗口
	prof := model.DefaultProfiles()[o.App]
	if prof != nil && prof.Critical {
		k := interKey{edge: l.edgeID, flow: o.Key}
		if !o.SLA {
			if _, ok := l.openInter[k]; !ok {
				in := Interruption{EdgeID: l.edgeID, App: o.App, Flow: o.Key, Start: now}
				l.openInter[k] = &in
				l.opened = append(l.opened, in)
			}
		} else if openIn, ok := l.openInter[k]; ok {
			openIn.End = now
			l.closed = append(l.closed, o.Key)
		}
	}
}

// AddFlowEnd records a flow termination, closing any open interruption.
func (l *LocalAcc) AddFlowEnd(now time.Time, k model.FlowKey) {
	key := interKey{edge: l.edgeID, flow: k}
	if _, ok := l.openInter[key]; ok {
		l.closeAt = now
		l.closed = append(l.closed, k)
	}
}

// AddEvents records migration events (flap detection happens at global merge).
func (l *LocalAcc) AddEvents(evs []model.FlowEvent) {
	for _, ev := range evs {
		if len(ev.Reason) >= 8 && ev.Reason[:8] == "migrate:" {
			l.migrateN++
		}
	}
	l.events = append(l.events, evs...)
}

// MarkAutonomous records one autonomous tick.
func (l *LocalAcc) MarkAutonomous() { l.autonomous = 1 }

// Merge folds this local accumulator into the global Metrics under one lock.
func (m *Metrics) Merge(l *LocalAcc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ticks++

	for i, app := range model.AllApps {
		am := m.App[app]
		am.Samples += l.appSamples[i]
		am.SLAMet += l.appSLAMet[i]
		am.InterruptedS += l.appInter[i]
		am.Throttled += l.appThrot[i]
		am.BytesOffered += l.appOffered[i]
		am.BytesDelivered += l.appDeliv[i]
	}
	for i, t := range model.AllLinkTypes {
		lm := m.Link[t]
		lm.BytesUp += l.linkBytes[i]
		lm.Replicated += l.linkReplicate[i]
		lm.Seconds += l.linkSeconds[i]
	}
	m.ClassSamples += l.classSamples
	m.ClassCorrect += l.classCorrect
	m.Replications += l.replications
	m.AutonomousS += l.autonomous
	m.Migrations += l.migrateN

	// flap 判定：同一流在 flapWindow 内迁回它刚离开的链路（乒乓），
	// 这才是滞回要抑制的来回切换
	for _, ev := range l.events {
		if len(ev.Reason) >= 8 && ev.Reason[:8] == "migrate:" {
			k := flowMigKey{edge: ev.EdgeID, flow: ev.Flow}
			if prev, ok := m.lastMigrate[k]; ok &&
				ev.To == prev.from && ev.At.Sub(prev.at) < flapWindow {
				m.Flaps++
			}
			m.lastMigrate[k] = migrationRec{from: ev.From, to: ev.To, at: ev.At}
		}
	}

	// 中断窗口：登记本 tick 新打开的，关闭已恢复/已结束的
	for i := range l.opened {
		in := l.opened[i]
		m.openInter[interKey{edge: in.EdgeID, flow: in.Flow}] = &Interruption{
			EdgeID: in.EdgeID, App: in.App, Flow: in.Flow, Start: in.Start,
		}
	}
	for _, k := range l.closed {
		key := interKey{edge: l.edgeID, flow: k}
		// 优先用本地已记录结束时刻的窗口
		if local, ok := l.openInter[key]; ok && !local.End.IsZero() {
			m.Interrupt = append(m.Interrupt, *local)
			delete(l.openInter, key)
			delete(m.openInter, key)
			continue
		}
		if open, ok := m.openInter[key]; ok {
			open.End = l.closeAt
			m.Interrupt = append(m.Interrupt, *open)
			delete(m.openInter, key)
			delete(l.openInter, key)
		}
	}
}

// CloseAll flushes this edge's still-open interruptions (run end).
func (l *LocalAcc) CloseAll(now time.Time) []Interruption {
	out := make([]Interruption, 0, len(l.openInter))
	for _, in := range l.openInter {
		in.End = now
		out = append(out, *in)
	}
	return out
}
