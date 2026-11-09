package dep

import a64 "sync/atomic"

// Gauge's array element is accessed through a renamed import.
type Gauge struct {
	n [2]uint64
}

func (g *Gauge) Load() uint64 { return a64.LoadUint64(&g.n[1]) }
