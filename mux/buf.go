package mux

import "sync"

// Frame payloads and queued receive data come from size-classed pools so idle
// streams hold no buffer and a burst does not churn the GC.
var classes = [...]int{256, 2048, 16 << 10, 64 << 10}

var pools [len(classes)]sync.Pool

type pbuf struct {
	b     []byte // full-capacity backing slice
	class int
}

func getBuf(n int) *pbuf {
	for i, c := range classes {
		if n <= c {
			if v := pools[i].Get(); v != nil {
				pb := v.(*pbuf)
				return pb
			}
			return &pbuf{b: make([]byte, c), class: i}
		}
	}
	// payloads are capped at 64 KiB-1, so this is unreachable for valid frames
	return &pbuf{b: make([]byte, n), class: -1}
}

func putBuf(pb *pbuf) {
	if pb == nil || pb.class < 0 {
		return
	}
	pools[pb.class].Put(pb)
}
