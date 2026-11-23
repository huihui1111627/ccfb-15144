// Package web serves the live SD-WAN dashboard: per-store/per-link real-time
// quality, application-to-link traffic distribution, flow migration events and
// manual fault injection against a running packet-level simulation.
package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"sdwan/internal/edge"
	"sdwan/internal/model"
	"sdwan/internal/network"
	"sdwan/internal/sim"
)

//go:embed static
var staticFS embed.FS

// Demo runs the packet-level simulation shown on the dashboard.
type Demo struct {
	mu      sync.Mutex
	tp      *network.Topology
	engines map[string]*sim.PacketEngine
	events  []model.FlowEvent
	now     time.Time
	t0      time.Time
}

const demoStores = 12 // 10 区域中选 12 家，含 2 家带移动网卡的重点店

// NewDemo builds the live simulation.
func NewDemo() *Demo {
	// 从晚间 19:30 开始，便于直接观察宽带晚高峰劣化
	t0 := time.Date(2026, 11, 23, 19, 30, 0, 0, time.UTC)
	tp := network.BuildTopology(300, 99)
	d := &Demo{tp: tp, engines: map[string]*sim.PacketEngine{}, now: t0, t0: t0}

	// 选前 demoStores 家，保证各区域都有代表
	picked := 0
	for i := 0; picked < demoStores; i += 25 {
		st := tp.Stores[i]
		hub := tp.HubByRegion[st.Info.Region]
		pe := sim.NewPacketEngine(st, hub, edge.ModeSDWAN, t0, int64(1000+i))
		pe.OnEvent(func(ev model.FlowEvent) {
			d.mu.Lock()
			d.events = append(d.events, ev)
			if len(d.events) > 300 {
				d.events = d.events[len(d.events)-300:]
			}
			d.mu.Unlock()
		})
		d.engines[st.Info.ID] = pe
		picked++
	}
	return d
}

// Start ticks the packet engine once per wall-clock second.
func (d *Demo) Start() {
	go func() {
		ticker := time.NewTicker(time.Second)
		for range ticker.C {
			d.Step()
		}
	}()
}

// Step advances one virtual second across all demo stores.
func (d *Demo) Step() {
	d.mu.Lock()
	d.now = d.now.Add(time.Second)
	now := d.now
	engs := make([]*sim.PacketEngine, 0, len(d.engines))
	for _, pe := range d.engines {
		engs = append(engs, pe)
	}
	d.mu.Unlock()
	for _, pe := range engs {
		pe.Tick(now)
	}
}

// Stores returns the demo store list with link info.
func (d *Demo) Stores() []model.StoreInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]model.StoreInfo, 0, len(d.engines))
	for id := range d.engines {
		if st := d.tp.Store(id); st != nil {
			out = append(out, st.Info)
		}
	}
	return out
}

// storeView is the JSON payload for one store.
type storeView struct {
	Info    model.StoreInfo                            `json:"info"`
	Links   map[model.LinkType]linkView                `json:"links"`
	Flows   []flowView                                 `json:"flows"`
	Stats   map[model.LinkType]sim.PacketStats         `json:"packet_stats"`
	Events  []model.FlowEvent                          `json:"events"`
	AppDist map[model.LinkType]map[model.AppID]float64 `json:"app_dist_bps"`
}

type linkView struct {
	Quality model.Quality `json:"quality"`
	BytesUp int64         `json:"bytes_up"`
	Cost    float64       `json:"cost"`
}

type flowView struct {
	Key     model.FlowKey  `json:"key"`
	App     model.AppID    `json:"app"`
	Primary model.LinkType `json:"primary"`
	Replica model.LinkType `json:"replica,omitempty"`
	Bps     float64        `json:"bps"`
	Why     string         `json:"class_why"`
}

func (d *Demo) viewStore(id string) (storeView, bool) {
	pe, ok := d.engines[id]
	if !ok {
		return storeView{}, false
	}
	st := d.tp.Store(id)
	v := storeView{
		Info:    st.Info,
		Links:   map[model.LinkType]linkView{},
		Stats:   pe.Stats(),
		AppDist: map[model.LinkType]map[model.AppID]float64{},
	}
	for _, t := range st.Info.Links {
		tr := pe.Engine().Tracker(t)
		q := tr.Quality()
		bytes := pe.Engine().BytesUp()
		v.Links[t] = linkView{Quality: q, BytesUp: bytes[t],
			Cost: float64(bytes[t]) / 1e9 * model.DefaultPolicy().CostPerGB[t]}
		v.AppDist[t] = map[model.AppID]float64{}
	}
	flowBps := pe.FlowBps()
	for _, f := range pe.Engine().Flows() {
		v.Flows = append(v.Flows, flowView{
			Key: f.Key, App: f.App, Primary: f.Primary,
			Replica: f.Replica, Why: f.ClassWhy, Bps: flowBps[f.Key],
		})
	}
	// 应用-链路分布：按各流当前实测速率归集到主链路
	for _, fv := range v.Flows {
		if fv.Bps <= 0 {
			continue
		}
		v.AppDist[fv.Primary][fv.App] += fv.Bps
	}
	d.mu.Lock()
	evs := make([]model.FlowEvent, 0)
	for _, ev := range d.events {
		if ev.EdgeID == id {
			evs = append(evs, ev)
		}
	}
	if len(evs) > 40 {
		evs = evs[len(evs)-40:]
	}
	d.mu.Unlock()
	v.Events = evs
	return v, true
}

// FaultRequest is the dashboard fault-injection body.
type FaultRequest struct {
	StoreID string         `json:"store_id"` // 空 = 全部演示门店
	Link    model.LinkType `json:"link"`
	Action  string         `json:"action"` // "outage" | "restore" | "degrade" | "restore-cap"
	Mult    float64        `json:"mult"`
}

func (d *Demo) inject(req FaultRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, pe := range d.engines {
		if req.StoreID != "" && req.StoreID != id {
			continue
		}
		link := d.tp.Link(id, req.Link)
		if link == nil {
			continue
		}
		switch req.Action {
		case "outage":
			link.SetOutage(true)
		case "restore":
			link.SetOutage(false)
		case "degrade":
			link.SetCapacityOverride(req.Mult)
		case "restore-cap":
			link.ClearCapacityOverride()
		}
		_ = pe
	}
}

// Handler builds the HTTP mux.
func (d *Demo) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/stores", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.Stores())
	})
	mux.HandleFunc("/api/store", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		v, ok := d.viewStore(id)
		if !ok {
			http.Error(w, "unknown store", http.StatusNotFound)
			return
		}
		writeJSON(w, v)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		evs := append([]model.FlowEvent(nil), d.events...)
		if len(evs) > 100 {
			evs = evs[len(evs)-100:]
		}
		d.mu.Unlock()
		writeJSON(w, evs)
	})
	mux.HandleFunc("/api/now", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		now := d.now
		d.mu.Unlock()
		writeJSON(w, map[string]string{"now": now.Format(time.RFC3339)})
	})
	mux.HandleFunc("/api/fault", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "post only", http.StatusMethodNotAllowed)
			return
		}
		var req FaultRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Mult <= 0 {
			req.Mult = 0.25
		}
		d.inject(req)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/policy", func(w http.ResponseWriter, r *http.Request) {
		// 演示页面展示当前策略（所有边缘策略一致）
		writeJSON(w, model.DefaultPolicy())
	})

	sub, _ := fs.Sub(staticFS, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
