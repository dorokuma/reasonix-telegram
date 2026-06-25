// Network configuration for reasonix-telegram.
// Uses system DNS (cgo) so that all traffic goes through the dae transparent proxy.
// No IPv4-preference or fallback-IP logic — IPv6 AAAA records are resolved normally.
package main

import (
	"net"
)

func init() {
	// Use system DNS (cgo) instead of Go's built-in resolver.
	// This ensures DNS queries go through the dae TProxy, and that
	// IPv6 AAAA records are returned when available.
	net.DefaultResolver = &net.Resolver{
		PreferGo: false,
	}
}
