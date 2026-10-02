// Package clientip picks the client address off a request, a configured header first.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// FromRequest is the address in header, else the peer address. A multi-valued
// header yields its last element, the one the trusted edge appended; anything
// before it came from the client. Anything that is not an IP literal, or a
// zoned IPv6 literal, falls back to the peer.
func FromRequest(r *http.Request, header string) string {
	if header != "" {
		if vs := r.Header.Values(header); len(vs) > 0 {
			parts := strings.Split(vs[len(vs)-1], ",")
			if ip, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil && ip.Zone() == "" {
				return ip.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return ""
	}
	return ip.Unmap().String()
}
