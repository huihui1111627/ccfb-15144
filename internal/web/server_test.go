package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sdwan/internal/model"
)

func TestDemoStoresAndSnapshot(t *testing.T) {
	d := NewDemo()
	stores := d.Stores()
	if len(stores) == 0 {
		t.Fatal("no demo stores")
	}
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/stores")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var got []model.StoreInfo
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(stores) {
		t.Fatalf("stores %d vs %d", len(got), len(stores))
	}

	// 单店视图
	resp2, err := http.Get(srv.URL + "/api/store?id=" + got[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("store status %d", resp2.StatusCode)
	}
	var v storeView
	if err := json.NewDecoder(resp2.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	if len(v.Links) < 2 {
		t.Fatalf("expected >=2 links, got %d", len(v.Links))
	}

	// 静态首页
	resp3, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("index status %d", resp3.StatusCode)
	}
}

func TestDemoFaultInjection(t *testing.T) {
	d := NewDemo()
	stores := d.Stores()
	id := stores[0].ID
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()

	body := `{"store_id":"` + id + `","link":"broadband","action":"outage"}`
	resp, err := http.Post(srv.URL+"/api/fault", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("fault status %d", resp.StatusCode)
	}
	link := d.tp.Link(id, model.LinkBroadband)
	if !link.Outage() {
		t.Fatal("outage not applied")
	}

	// 恢复
	body2 := `{"store_id":"` + id + `","link":"broadband","action":"restore"}`
	resp2, _ := http.Post(srv.URL+"/api/fault", "application/json", strings.NewReader(body2))
	resp2.Body.Close()
	if link.Outage() {
		t.Fatal("outage not cleared")
	}
}

func TestDemoAdvancesTime(t *testing.T) {
	d := NewDemo()
	before := d.now
	d.Step()
	if !d.now.After(before) {
		t.Fatalf("time did not advance: %s -> %s", before, d.now)
	}
	_ = time.Second
}
