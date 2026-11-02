// Package netsafe is the connection-time address policy shared by the custom http
// tools (internal/tool) and the remote MCP transports (internal/mcp): the dialer
// resolves the host itself, refuses every address the policy forbids and connects
// to the address it validated, so a DNS answer (or a redirect target) cannot move a
// request to an internal or metadata address after a check on the name.
package netsafe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"
)

func mustPrefixes(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

var (
	// always blocked, even with allowPrivate
	alwaysBlocked = mustPrefixes("0.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4",
		"fe80::/10", "ff00::/8", "fd00:ec2::/32", "100.100.100.200/32", "168.63.129.16/32", "::/128")
	// blocked unless allowPrivate
	privateBlocked = mustPrefixes("127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
		"192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"::1/128", "fc00::/7", "64:ff9b::/96", "2002::/16", "2001:db8::/32")
)

// Blocked reports whether connecting to ip is forbidden: link-local, multicast,
// unspecified and cloud metadata addresses always; loopback, private, CGNAT and
// documentation ranges unless allowPrivate.
func Blocked(ip net.IP, allowPrivate bool) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap().WithZone("")
	for _, p := range alwaysBlocked {
		if p.Contains(a) {
			return true
		}
	}
	// An IPv6 address that embeds an IPv4 address (NAT64, 6to4, IPv4-compatible, SIIT)
	// is judged by the embedded address too, so 64:ff9b::a9fe:a9fe cannot reach
	// 169.254.169.254 through a translator.
	if v4, ok := embeddedIPv4(a); ok && Blocked(v4.AsSlice(), allowPrivate) {
		return true
	}
	if allowPrivate {
		return false
	}
	for _, p := range privateBlocked {
		if p.Contains(a) {
			return true
		}
	}
	return a.IsLoopback() || a.IsPrivate()
}

// embeddedIPv4 extracts the IPv4 address embedded in NAT64 (64:ff9b::/96), 6to4
// (2002::/16), IPv4-compatible (::a.b.c.d) and SIIT (::ffff:0:a.b.c.d) addresses.
func embeddedIPv4(a netip.Addr) (netip.Addr, bool) {
	if !a.Is6() {
		return netip.Addr{}, false
	}
	b := a.As16()
	zero := func(from, to int) bool {
		for _, x := range b[from:to] {
			if x != 0 {
				return false
			}
		}
		return true
	}
	switch {
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && zero(4, 12): // 64:ff9b::/96
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case b[0] == 0x20 && b[1] == 0x02: // 2002::/16, the IPv4 address follows the prefix
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	case zero(0, 12) && b[12] != 0: // ::a.b.c.d (not :: and not ::1)
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case zero(0, 8) && b[8] == 0xff && b[9] == 0xff && b[10] == 0 && b[11] == 0: // ::ffff:0:a.b.c.d
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	}
	return netip.Addr{}, false
}

// BlockedError is returned by the dialer when a resolved address is not allowed.
type BlockedError struct {
	Host string
	IP   net.IP
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("SSRF 방어: %s (%s) 는 허용되지 않는 주소입니다 (사설/루프백/링크로컬/메타데이터). 사설망은 allow_private 정책으로 명시 허용해야 하며 링크로컬·메타데이터는 항상 차단됩니다", e.Host, e.IP)
}

// Lookup resolves a host name to addresses (tests replace it).
type Lookup func(ctx context.Context, host string) ([]net.IP, error)

// DefaultLookup resolves with the system resolver.
func DefaultLookup(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

// DialContext returns a dialer that resolves the host itself and validates every
// address at connect time (no DNS rebinding window: the validated IP is the one
// dialed). A nil lookup uses DefaultLookup.
func DialContext(allowPrivate bool, lookup Lookup) func(context.Context, string, string) (net.Conn, error) {
	if lookup == nil {
		lookup = DefaultLookup
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := lookup(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("호스트 %s 를 해석할 수 없습니다", host)
		}
		for _, ip := range ips {
			if Blocked(ip, allowPrivate) {
				return nil, &BlockedError{Host: host, IP: ip}
			}
		}
		d := net.Dialer{Timeout: 10 * time.Second}
		var last error
		for _, ip := range ips {
			c, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

// IsLocalName reports whether host names this machine explicitly: "localhost",
// "*.localhost" or a literal loopback address.
func IsLocalName(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// IsLiteralPrivate reports whether host is a literal IP address that is loopback or private
// (an explicit choice of the person who wrote the URL, as opposed to a name that
// merely resolves there).
func IsLiteralPrivate(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return Blocked(ip, false) && !Blocked(ip, true)
}
