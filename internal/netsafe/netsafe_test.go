package netsafe

import (
	"net"
	"testing"
)

func TestBlockedPolicy(t *testing.T) {
	for ip, want := range map[string][2]bool{ // {blocked by default, blocked with allowPrivate}
		"8.8.8.8":         {false, false},
		"127.0.0.1":       {true, false},
		"10.1.2.3":        {true, false},
		"192.168.0.1":     {true, false},
		"::1":             {true, false},
		"169.254.169.254": {true, true},
		"fe80::1":         {true, true},
		"0.0.0.0":         {true, true},
		"fd00:ec2::254":   {true, true},
		"::ffff:10.0.0.1": {true, false}, // IPv4-mapped is judged as IPv4
		"168.63.129.16":   {true, true},  // Azure wire server / metadata
		// embedded IPv4: judged by the embedded address
		"64:ff9b::a9fe:a9fe":       {true, true},  // NAT64 of 169.254.169.254
		"64:ff9b::808:808":         {true, false}, // NAT64 of 8.8.8.8 stays blocked by default only (translation prefix)
		"2002:a9fe:a9fe::1":        {true, true},  // 6to4 of 169.254.169.254
		"2002:7f00:1::1":           {true, false}, // 6to4 of 127.0.0.1
		"::169.254.169.254":        {true, true},  // IPv4-compatible
		"::10.0.0.1":               {true, false},
		"::ffff:0:169.254.169.254": {true, true}, // SIIT
		"::ffff:0:10.0.0.1":        {true, false},
		"::ffff:0:8.8.8.8":         {false, false},
	} {
		if got := [2]bool{Blocked(net.ParseIP(ip), false), Blocked(net.ParseIP(ip), true)}; got != want {
			t.Errorf("%s: blocked(default, allowPrivate) = %v, want %v", ip, got, want)
		}
	}
}

func TestLocalNames(t *testing.T) {
	for h, want := range map[string]bool{"localhost": true, "LOCALHOST.": true, "a.localhost": true, "127.0.0.1": true, "::1": true,
		"10.0.0.5": false, "example.com": false, "localhost.example.com": false} {
		if got := IsLocalName(h); got != want {
			t.Errorf("IsLocalName(%q) = %v", h, got)
		}
	}
	for h, want := range map[string]bool{"10.0.0.5": true, "192.168.1.1": true, "127.0.0.1": true, "[::1]": true, "8.8.8.8": false,
		"169.254.169.254": false, "intranet.corp": false} {
		if got := IsLiteralPrivate(h); got != want {
			t.Errorf("IsLiteralPrivate(%q) = %v", h, got)
		}
	}
}
