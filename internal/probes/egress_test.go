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
	for _, address := range []string{"[fe80::1%eth0]:80", "[::1%lo0]:80", "[::ffff:127.0.0.1]:80"} {
		if err := (Egress{Denied: denied}).control("tcp6", address, nil); !errors.Is(err, ErrTargetDenied) {
			t.Errorf("%s: got %v, want target denied", address, err)
		}
	}
}
