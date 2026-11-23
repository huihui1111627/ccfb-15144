package edge

import (
	"testing"
	"time"

	"sdwan/internal/model"
)

func TestBulkFallbackToMPLSWhenCheapDown(t *testing.T) {
	links := testLinks(t)
	e := NewEngine("store-0001", ModeSDWAN, model.DefaultPolicy(), links)
	// 宽带和移动网都中断，只剩专线
	links[1].SetOutage(true) // broadband
	links[2].SetOutage(true) // cellular

	k := testKey(model.AppBulk, "10.10.0.5", 8443)
	e.AdmitFlow(NewFlow{Key: k, First: true})
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	var last TickOutput
	for i := 0; i < 35; i++ {
		now = now.Add(time.Second)
		for _, o := range e.Tick(now, []TickInput{{Key: k, Demand: 100_000}}) {
			last = o
		}
	}
	if last.Primary != model.LinkMPLS {
		t.Fatalf("bulk fallback = %s, want mpls", last.Primary)
	}
}

func TestBulkPrefersBroadbandOverCellular(t *testing.T) {
	links := testLinks(t)
	e := NewEngine("store-0001", ModeSDWAN, model.DefaultPolicy(), links)
	k := testKey(model.AppBulk, "10.10.0.5", 8443)
	e.AdmitFlow(NewFlow{Key: k, First: true})
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	var last TickOutput
	for i := 0; i < 35; i++ {
		now = now.Add(time.Second)
		for _, o := range e.Tick(now, []TickInput{{Key: k, Demand: 100_000}}) {
			last = o
		}
	}
	if last.Primary != model.LinkBroadband {
		t.Fatalf("bulk = %s, want broadband", last.Primary)
	}
}

func TestShapedBulkNotSLAViolation(t *testing.T) {
	links := testLinks(t)
	pol := model.DefaultPolicy()
	// 令牌桶配额 = cap/8 字节/tick；cap=1 时配额向下取整为 0，所有 bulk 被整形
	pol.BulkCapBps = 1
	e := NewEngine("store-0001", ModeSDWAN, pol, links)
	k := testKey(model.AppBulk, "10.10.0.5", 8443)
	e.AdmitFlow(NewFlow{Key: k, First: true})
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	var shapedTicks, violated int
	for i := 0; i < 35; i++ {
		now = now.Add(time.Second)
		for _, o := range e.Tick(now, []TickInput{{Key: k, Demand: 500_000}}) {
			if o.Why == "shaped" {
				shapedTicks++
				if !o.SLA {
					violated++
				}
			}
		}
	}
	if shapedTicks == 0 {
		t.Fatal("expected shaping to kick in")
	}
	if violated != 0 {
		t.Fatalf("shaped bulk ticks counted as SLA violations: %d", violated)
	}
}

func TestNoReplicationForNonCritical(t *testing.T) {
	links := testLinks(t)
	pol := model.DefaultPolicy()
	e := NewEngine("store-0001", ModeSDWAN, pol, links)
	// 全链路高丢包
	for _, l := range links {
		cfg := l.Config()
		cfg.BaseLoss = 0.3
		l.SetConfig(cfg)
	}
	k := testKey(model.AppWeb, "172.16.4.10", 443)
	e.AdmitFlow(NewFlow{Key: k, First: true})
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	var last TickOutput
	for i := 0; i < 35; i++ {
		now = now.Add(time.Second)
		for _, o := range e.Tick(now, []TickInput{{Key: k, Demand: 10_000}}) {
			last = o
		}
	}
	if last.Replica != "" {
		t.Fatalf("non-critical web flow must not be replicated, replica=%s", last.Replica)
	}
}
