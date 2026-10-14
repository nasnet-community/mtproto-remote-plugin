//go:build !linux

package daemon

func rss() uint64 { return 0 }
