package controller

import (
	"testing"

	"sdwan/internal/model"
)

func TestPolicyVersioningAndPull(t *testing.T) {
	c := New(model.DefaultPolicy())
	p1 := c.CurrentPolicy()
	if p1.Version != 1 {
		t.Fatalf("seed version = %d", p1.Version)
	}
	p2 := model.DefaultPolicy()
	p2.BulkCapBps = 2_000_000
	pushed := c.PushPolicy(p2)
	if pushed.Version != 2 {
		t.Fatalf("pushed version = %d", pushed.Version)
	}
	got, err := c.PullPolicy("store-0001")
	if err != nil || got.BulkCapBps != 2_000_000 {
		t.Fatalf("pull failed: %v %+v", err, got)
	}
}

func TestUnreachableServesLastPolicy(t *testing.T) {
	c := New(model.DefaultPolicy())
	client := NewInProcessClient(c, model.DefaultPolicy())
	p, err := client.Pull("store-0001")
	if err != nil || p == nil {
		t.Fatal("first pull should succeed")
	}
	c.SetLinkDown("store-0001", true)
	cached, err := client.Pull("store-0001")
	if err != ErrUnreachable {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	if cached == nil || cached.Version == 0 {
		t.Fatal("edge must keep its last policy when controller is down")
	}
	// 遥测上报失败但不致命
	if err := client.Report(model.EdgeTelemetry{EdgeID: "store-0001"}); err != ErrUnreachable {
		t.Fatalf("report error = %v", err)
	}
	c.SetLinkDown("store-0001", false)
	if _, err := client.Pull("store-0001"); err != nil {
		t.Fatal("pull should recover")
	}
}

func TestTelemetryAggregation(t *testing.T) {
	c := New(model.DefaultPolicy())
	_ = c.Report(model.EdgeTelemetry{EdgeID: "store-0001"})
	_ = c.Report(model.EdgeTelemetry{EdgeID: "store-0002"})
	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot size = %d", len(snap))
	}
	if _, ok := c.Edge("store-0001"); !ok {
		t.Fatal("store-0001 missing")
	}
}
