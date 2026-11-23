package sim

import (
	"math/rand"

	"sdwan/internal/model"
)

// knownDst returns a destination endpoint for a known application class. The
// addresses match the topology server catalog and the default classifier
// rules, so rule/DNS/signature paths all get exercised.
func knownDst(app model.AppID, rng *rand.Rand) model.Endpoint {
	switch app {
	case model.AppPOS:
		choices := []model.Endpoint{
			{IP: "10.8.0.11", Port: 443},    // 命中端口规则
			{IP: "100.64.10.7", Port: 7000}, // 端口规则 + DNS 映射
			{IP: "100.64.10.8", Port: 8443}, // 仅 DNS 关联（pay 域名解析出的第二个 IP）
		}
		return choices[rng.Intn(len(choices))]
	case model.AppVC:
		choices := []model.Endpoint{
			{IP: "10.9.0.20", Port: 5004},
			{IP: "10.9.0.21", Port: 5060},
			{IP: "10.9.0.22", Port: 5005},
		}
		return choices[rng.Intn(len(choices))]
	case model.AppBulk:
		choices := []model.Endpoint{
			{IP: "10.10.0.5", Port: 8443},
			{IP: "10.10.2.9", Port: 443},
			{IP: "10.10.2.10", Port: 80},
		}
		return choices[rng.Intn(len(choices))]
	case model.AppWeb:
		choices := []model.Endpoint{
			{IP: "172.16.4.10", Port: 443},
			{IP: "172.16.4.11", Port: 80},
		}
		return choices[rng.Intn(len(choices))]
	default:
		return model.Endpoint{IP: "198.51.100.23", Port: 9201}
	}
}
