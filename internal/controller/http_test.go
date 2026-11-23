package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"sdwan/internal/model"
)

func TestHTTPRoundTrip(t *testing.T) {
	c := New(model.DefaultPolicy())
	mux := http.NewServeMux()
	c.RegisterHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewHTTPClient(srv.URL, "store-0001", model.DefaultPolicy())
	p, err := client.Pull("store-0001")
	if err != nil || p == nil {
		t.Fatalf("pull failed: %v", err)
	}
	if err := client.Report(model.EdgeTelemetry{EdgeID: "store-0001"}); err != nil {
		t.Fatalf("report failed: %v", err)
	}
	if _, ok := c.Edge("store-0001"); !ok {
		t.Fatal("telemetry not aggregated")
	}

	// 下发新策略
	newPolicy := model.DefaultPolicy()
	newPolicy.BulkCapBps = 1_234_567
	body, _ := json.Marshal(newPolicy)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/policy", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got, err := client.Pull("store-0001")
	if err != nil {
		t.Fatal(err)
	}
	if got.BulkCapBps != 1_234_567 {
		t.Fatalf("policy push not propagated: %+v", got.BulkCapBps)
	}
}

func TestHTTPClientFallbackWhenServerDown(t *testing.T) {
	// 指向一个不存在的端口
	client := NewHTTPClient("http://127.0.0.1:1", "edge-x", model.DefaultPolicy())
	p, err := client.Pull("edge-x")
	if err != ErrUnreachable {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
	if p == nil || p.Version == 0 {
		t.Fatal("client must return fallback policy when unreachable")
	}
}

func TestStateEndpoint(t *testing.T) {
	c := New(model.DefaultPolicy())
	_ = c.Report(model.EdgeTelemetry{EdgeID: "a"})
	_ = c.Report(model.EdgeTelemetry{EdgeID: "b"})
	mux := http.NewServeMux()
	c.RegisterHTTP(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap map[string]model.EdgeTelemetry
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if len(snap) != 2 {
		t.Fatalf("state size = %d", len(snap))
	}
}
