package network

import (
	"fmt"
	"math/rand"

	"sdwan/internal/model"
)

// Store is one simulated retail store with an SD-WAN edge device.
type Store struct {
	Info  model.StoreInfo
	Links map[model.LinkType]*LinkModel
}

// Hub is a regional warehouse / regional DC terminating overlay tunnels.
type Hub struct {
	ID     string
	Region int
	IP     string
}

// Topology is the whole simulated network.
type Topology struct {
	Stores []*Store
	Hubs   []*Hub
	// HubByRegion maps region index -> hub.
	HubByRegion map[int]*Hub
	// Server catalog: app -> destination IP/port sets used by traffic generators
	// and mirrored in the default classifier rules.
	Servers map[model.AppID][]model.Endpoint
	// rng used only for topology construction.
	rng *rand.Rand
}

const regions = 10

// BuildTopology constructs nStores stores across 10 regions. Roughly 40% of
// stores also have a cellular NIC. Link parameters vary per store from a
// seeded distribution so the network does not look homogeneous.
func BuildTopology(nStores int, seed int64) *Topology {
	rng := rand.New(rand.NewSource(seed))
	tp := &Topology{
		HubByRegion: map[int]*Hub{},
		Servers:     map[model.AppID][]model.Endpoint{},
		rng:         rng,
	}

	for r := 0; r < regions; r++ {
		h := &Hub{ID: fmt.Sprintf("hub-r%02d", r), Region: r, IP: fmt.Sprintf("10.%d.0.1", r)}
		tp.Hubs = append(tp.Hubs, h)
		tp.HubByRegion[r] = h
	}

	// 服务器编址与默认分类规则保持一致
	tp.Servers[model.AppPOS] = []model.Endpoint{
		{IP: "10.8.0.11", Port: 443},
		{IP: "100.64.10.7", Port: 7000},
	}
	tp.Servers[model.AppVC] = []model.Endpoint{
		{IP: "10.9.0.20", Port: 5004},
		{IP: "10.9.0.21", Port: 5060},
	}
	tp.Servers[model.AppBulk] = []model.Endpoint{
		{IP: "10.10.0.5", Port: 8443},
		{IP: "10.10.2.9", Port: 443},
	}
	tp.Servers[model.AppWeb] = []model.Endpoint{
		{IP: "172.16.4.10", Port: 443},
		{IP: "172.16.4.11", Port: 80},
	}
	// 无规则匹配的“未知”服务，用来验证行为归类
	tp.Servers[model.AppGeneral] = []model.Endpoint{
		{IP: "198.51.100.23", Port: 9201},
		{IP: "203.0.113.77", Port: 6123},
	}

	for i := 0; i < nStores; i++ {
		region := i % regions
		id := fmt.Sprintf("store-%04d", i+1)
		hasCell := rng.Float64() < 0.4
		subnet := fmt.Sprintf("192.168.%d.", i%250+1)
		st := &Store{
			Info: model.StoreInfo{
				ID: id, Region: region, Subnet: subnet,
				HasCell: hasCell,
			},
			Links: map[model.LinkType]*LinkModel{},
		}

		st.Links[model.LinkMPLS] = NewLink(LinkConfig{
			ID:   model.LinkID{EdgeID: id, Type: model.LinkMPLS},
			Type: model.LinkMPLS,
			// 专线：低时延低丢包，带宽小且贵
			BaseRTT:  18 + rng.Float64()*10,
			BaseJit:  3 + rng.Float64()*2,
			BaseLoss: 0.0005 + rng.Float64()*0.001,
			UpBps:    8_000_000 + rng.Float64()*8_000_000,
			DownBps:  20_000_000 + rng.Float64()*20_000_000,
			Seed:     seed + int64(i)*7 + 1,
		})
		st.Links[model.LinkBroadband] = NewLink(LinkConfig{
			ID:   model.LinkID{EdgeID: id, Type: model.LinkBroadband},
			Type: model.LinkBroadband,
			// 宽带：平时 RTT 偏高、抖动大，但带宽大
			BaseRTT:  35 + rng.Float64()*25,
			BaseJit:  8 + rng.Float64()*8,
			BaseLoss: 0.002 + rng.Float64()*0.004,
			UpBps:    30_000_000 + rng.Float64()*70_000_000,
			DownBps:  100_000_000 + rng.Float64()*200_000_000,
			Seed:     seed + int64(i)*7 + 2,
		})
		if hasCell {
			st.Links[model.LinkCellular] = NewLink(LinkConfig{
				ID:       model.LinkID{EdgeID: id, Type: model.LinkCellular},
				Type:     model.LinkCellular,
				BaseRTT:  45 + rng.Float64()*30,
				BaseJit:  10 + rng.Float64()*10,
				BaseLoss: 0.004 + rng.Float64()*0.008,
				UpBps:    10_000_000 + rng.Float64()*20_000_000,
				DownBps:  30_000_000 + rng.Float64()*40_000_000,
				Seed:     seed + int64(i)*7 + 3,
			})
			st.Info.Links = append(st.Info.Links, model.LinkCellular)
		}
		st.Info.Links = append([]model.LinkType{model.LinkMPLS, model.LinkBroadband}, st.Info.Links...)
		tp.Stores = append(tp.Stores, st)
	}
	return tp
}

// Link returns one store's link model.
func (tp *Topology) Link(storeID string, t model.LinkType) *LinkModel {
	for _, s := range tp.Stores {
		if s.Info.ID == storeID {
			return s.Links[t]
		}
	}
	return nil
}

// Store finds a store by id.
func (tp *Topology) Store(id string) *Store {
	for _, s := range tp.Stores {
		if s.Info.ID == id {
			return s
		}
	}
	return nil
}
