package sim

import (
	"testing"
	"time"

	"sdwan/internal/model"
)

func TestFlapCountsOnlyPingPong(t *testing.T) {
	m := NewMetrics()
	k := model.FlowKey{Proto: "tcp", Dst: model.Endpoint{IP: "10.8.0.11", Port: 443}}
	t0 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	// 第一次迁移 mpls -> broadband（不构成乒乓）
	acc1 := NewLocalAcc("store-0001")
	acc1.AddEvents([]model.FlowEvent{{
		At: t0, EdgeID: "store-0001", Flow: k, From: model.LinkMPLS,
		To: model.LinkBroadband, Reason: "migrate:sla-best-score",
	}})
	m.Merge(acc1)
	if m.Flaps != 0 || m.Migrations != 1 {
		t.Fatalf("first migration: flaps=%d migrations=%d", m.Flaps, m.Migrations)
	}

	// 30 秒内回迁 broadband -> mpls（乒乓，应计数）
	acc2 := NewLocalAcc("store-0001")
	acc2.AddEvents([]model.FlowEvent{{
		At: t0.Add(30 * time.Second), EdgeID: "store-0001", Flow: k,
		From: model.LinkBroadband, To: model.LinkMPLS,
		Reason: "migrate:best-effort-sla-violation",
	}})
	m.Merge(acc2)
	if m.Flaps != 1 {
		t.Fatalf("ping-pong within window should count: flaps=%d", m.Flaps)
	}

	// 5 分钟后再次迁走（超过窗口，不算乒乓）
	acc3 := NewLocalAcc("store-0001")
	acc3.AddEvents([]model.FlowEvent{{
		At: t0.Add(6 * time.Minute), EdgeID: "store-0001", Flow: k,
		From: model.LinkMPLS, To: model.LinkBroadband,
		Reason: "migrate:sla-best-score",
	}})
	m.Merge(acc3)
	if m.Flaps != 1 {
		t.Fatalf("migration outside window must not flap: flaps=%d", m.Flaps)
	}
	if m.Migrations != 3 {
		t.Fatalf("migrations=%d, want 3", m.Migrations)
	}

	// 迁到第三条链路（不是回刚离开的），不算乒乓
	acc4 := NewLocalAcc("store-0001")
	acc4.AddEvents([]model.FlowEvent{{
		At: t0.Add(7 * time.Minute), EdgeID: "store-0001", Flow: k,
		From: model.LinkBroadband, To: model.LinkCellular,
		Reason: "migrate:sla-best-score",
	}})
	m.Merge(acc4)
	if m.Flaps != 1 {
		t.Fatalf("migration to a third link must not flap: flaps=%d", m.Flaps)
	}
}
