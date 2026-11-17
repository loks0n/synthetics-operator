package probes

import (
	"errors"
	"testing"
)

func TestDeniedNetworksIncludeScopedIPv6(t *testing.T) {
	denied, err := ParseDeniedNetworks("private")
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"[fe80::1%eth0]:80", "[::1%lo0]:80", "[::ffff:127.0.0.1]:80", "[::127.0.0.1]:80", "[::10.0.0.1]:80"} {
		if err := (Egress{Denied: denied}).control("tcp6", address, nil); !errors.Is(err, ErrTargetDenied) {
			t.Errorf("%s: got %v, want target denied", address, err)
		}
	}
}

// An IPv4-compatible address (::a.b.c.d) embeds an IPv4 address the way a
// mapped one does, so a denied IPv4 range must cover it too.
func TestDeniedIPv4NetworksCoverIPv4CompatibleAddresses(t *testing.T) {
	denied, err := ParseDeniedNetworks("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	egress := Egress{Denied: denied}
	if err := egress.control("tcp6", "[::192.0.2.10]:80", nil); !errors.Is(err, ErrTargetDenied) {
		t.Errorf("::192.0.2.10: got %v, want target denied", err)
	}
	for _, address := range []string{"[::198.51.100.10]:80", "[2001:db8::c000:20a]:80"} {
		if err := egress.control("tcp6", address, nil); err != nil {
			t.Errorf("%s: got %v, want allowed", address, err)
		}
	}
}
