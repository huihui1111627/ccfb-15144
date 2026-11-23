package sim

import (
	"testing"
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
	"sdwan/internal/network"
)

func TestPacketEngineMigrationOnOutage(t *testing.T) {
	tp := network.BuildTopology(4, 7)
	st := tp.Stores[0]
	hub := tp.HubByRegion[st.Info.Region]
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	pe := NewPacketEngine(st, hub, edge.ModeSDWAN, t0, 555)

	// 跑 40 秒建立探测基线和活动流
	now := t0
	for i := 0; i < 40; i++ {
		now = now.Add(time.Second)
		pe.Tick(now)
	}
	flows := pe.Engine().Flows()
	if len(flows) == 0 {
		t.Fatal("no active flows after warmup")
	}

	// 找到一条走宽带的非 bulk 流，记录其初始链路
	var target model.FlowKey
	var initial model.LinkType
	for _, f := range flows {
		if f.Primary == model.LinkBroadband {
			target, initial = f.Key, f.Primary
			break
		}
	}
	if initial == "" {
		t.Skip("no broadband flow to migrate; rerun seed")
	}

	// 切断宽带
	st.Links[model.LinkBroadband].SetOutage(true)
	migrated := false
	for i := 0; i < 40; i++ {
		now = now.Add(time.Second)
		pe.Tick(now)
		if f, ok := pe.Engine().Flow(target); ok && f.Active && f.Primary != model.LinkBroadband {
			migrated = true
			break
		}
	}
	if !migrated {
		t.Fatal("flow did not migrate off the outaged broadband link")
	}

	// 恢复宽带，确认链路重新可用
	st.Links[model.LinkBroadband].SetOutage(false)
	for i := 0; i < 40; i++ {
		now = now.Add(time.Second)
		pe.Tick(now)
	}
	q := pe.Engine().Tracker(model.LinkBroadband).Quality()
	if !q.Healthy {
		t.Fatal("broadband did not recover as healthy")
	}
}

func TestPacketEngineDedupCounters(t *testing.T) {
	tp := network.BuildTopology(4, 9)
	st := tp.Stores[0]
	hub := tp.HubByRegion[st.Info.Region]
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	pe := NewPacketEngine(st, hub, edge.ModeSDWAN, t0, 888)

	// 先验证去重器基本行为
	k := model.FlowKey{Proto: "tcp", Dst: model.Endpoint{IP: "10.8.0.11", Port: 443}}
	if !pe.Engine().DedupFirst(k, 1) {
		t.Fatal("first copy must pass")
	}
	if pe.Engine().DedupFirst(k, 1) {
		t.Fatal("duplicate must be suppressed")
	}

	// 跑一段时间，统计接收端计数器有数据
	now := t0
	for i := 0; i < 20; i++ {
		now = now.Add(time.Second)
		pe.Tick(now)
	}
	stats := pe.Stats()
	var delivered int64
	for _, s := range stats {
		delivered += s.Delivered
	}
	if delivered == 0 {
		t.Fatal("no packets delivered through packet engine")
	}
}
