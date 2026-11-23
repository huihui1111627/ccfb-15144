package probe

// Deterministic, allocation-free pseudo-random helpers keyed by probe sequence.
// Using a hash instead of a shared RNG keeps per-link probes independent and
// whole simulations reproducible.

func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// unitFloat returns a deterministic pseudo-random float in [0,1).
func unitFloat(seed uint64) float64 {
	return float64(mix64(seed)>>11) / float64(uint64(1)<<53)
}

func bernoulli(p float64, seq uint64) bool {
	return unitFloat(0x9e3779b97f4a7c15^mix64(seq)) < p
}

// normSample returns a deterministic approximately-normal sample (sum of
// three uniforms, centered) scaled to roughly N(0,1).
func normSample(seq uint64) float64 {
	base := mix64(seq)
	a := unitFloat(base + 1)
	b := unitFloat(base + 2)
	c := unitFloat(base + 3)
	return (a + b + c - 1.5) * 1.414
}
