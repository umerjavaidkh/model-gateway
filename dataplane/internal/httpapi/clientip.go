package httpapi

import (
	"net"
	"net/http"
	"net/netip"
)

// clientIP returns the peer address of the connection.
//
// It deliberately ignores X-Forwarded-For. That header is caller-controlled
// unless every hop in front of the gateway is trusted and rewrites it, and
// policy decisions — geo rules, IP allowlists — are made from this value. Until
// there is a configured list of trusted proxies, believing the header would
// hand callers a way to spoof their own source address.
func clientIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}
