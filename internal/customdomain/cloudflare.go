package customdomain

import (
	"net"
	"strings"
	"sync"
)

// Official Cloudflare anycast ranges. Orange-cloud records hide the CNAME
// and resolve to these addresses instead of Runex's origin.
var cloudflareCIDRs = []string{
	"173.245.48.0/20",
	"103.21.244.0/22",
	"103.22.200.0/22",
	"103.31.4.0/22",
	"141.101.64.0/18",
	"108.162.192.0/18",
	"190.93.240.0/20",
	"188.114.96.0/20",
	"197.234.240.0/22",
	"198.41.128.0/17",
	"162.158.0.0/15",
	"104.16.0.0/13",
	"104.24.0.0/14",
	"172.64.0.0/13",
	"131.0.72.0/22",
	"2400:cb00::/32",
	"2606:4700::/32",
	"2803:f800::/32",
	"2405:b500::/32",
	"2405:8100::/32",
	"2a06:98c0::/29",
	"2c0f:f248::/32",
}

var (
	cfNets     []*net.IPNet
	cfNetsOnce sync.Once
)

func cloudflareNets() []*net.IPNet {
	cfNetsOnce.Do(func() {
		for _, cidr := range cloudflareCIDRs {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				continue
			}
			cfNets = append(cfNets, network)
		}
	})
	return cfNets
}

func IPInCloudflare(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, network := range cloudflareNets() {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// CloudflareProxied is true when public A/AAAA for the hostname sit on
// Cloudflare anycast. That is what an orange-cloud CNAME looks like from
// the outside: no CNAME, only CF IPs.
func CloudflareProxied(ips []string) bool {
	seen := false
	for _, raw := range ips {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil {
			continue
		}
		if !IPInCloudflare(ip) {
			return false
		}
		seen = true
	}
	return seen
}
