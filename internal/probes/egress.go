package probes

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// ErrTargetDenied is returned, wrapped in the dial error, when a probe would
// connect to an address inside a denied network.
var ErrTargetDenied = errors.New("target address is in a denied network")

// privateNetworks are the ranges a public target never resolves to: private,
// loopback, link-local (including cloud metadata), CGNAT and unspecified.
func privateNetworks() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("::/128"),
		netip.MustParsePrefix("::1/128"),
		netip.MustParsePrefix("fc00::/7"),
		netip.MustParsePrefix("fe80::/10"),
	}
}

// Egress decides which addresses probes may connect to. The zero value allows
// everything, which is what a cluster probing its own services needs.
type Egress struct {
	Denied []netip.Prefix
}

// ParseDeniedNetworks reads a comma-separated list of CIDRs, where `private`
// stands for the private, loopback, link-local, CGNAT and unspecified ranges.
func ParseDeniedNetworks(value string) ([]netip.Prefix, error) {
	var denied []netip.Prefix
	for entry := range strings.SplitSeq(value, ",") {
		entry = strings.TrimSpace(entry)
		switch entry {
		case "":
			continue
		case "private":
			denied = append(denied, privateNetworks()...)
		default:
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("denied network %q: %w", entry, err)
			}
			denied = append(denied, prefix.Masked())
		}
	}
	return denied, nil
}

// dialer returns a net.Dialer that refuses denied addresses. The check runs on
// the resolved address of every connection, so a hostname that resolves into
// a denied network, a redirect to one, and DNS rebinding are all refused.
func (e Egress) dialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: e.control}
}

func (e Egress) control(_, address string, _ syscall.RawConn) error {
	if len(e.Denied) == 0 {
		return nil
	}
	addressPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("parse dial address %q: %w", address, err)
	}
	ip := addressPort.Addr().Unmap()
	for _, prefix := range e.Denied {
		if prefix.Contains(ip) {
			return fmt.Errorf("%w: %s", ErrTargetDenied, ip)
		}
	}
	return nil
}
