package probes

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

func tcpProbe(host string, port int32) *syntheticsv1alpha1.TCPProbe {
	return &syntheticsv1alpha1.TCPProbe{Spec: syntheticsv1alpha1.TCPProbeSpec{
		Timeout: metav1.Duration{Duration: time.Second},
		Target:  syntheticsv1alpha1.TCPTarget{Host: host, Port: port},
	}}
}

func TestTCPExecutorConnectsClosesAndSendsNoData(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	serverResult := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 1)
		n, err := conn.Read(buf)
		if n != 0 {
			serverResult <- errors.New("TCPProbe sent application data")
			return
		}
		serverResult <- err
	}()

	addr := listener.Addr().(*net.TCPAddr)
	r := (TCPExecutor{}).executeProbe(t.Context(), tcpProbe(addr.IP.String(), int32(addr.Port)))
	if !r.success() {
		t.Fatalf("expected success, got %+v", r)
	}
	if err := <-serverResult; !errors.Is(err, net.ErrClosed) && err != nil {
		// A peer close is normally io.EOF; any error with zero bytes proves no
		// payload was sent and is acceptable for platform-specific sockets.
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("probe did not close connection promptly: %v", err)
		}
	}
}

type errorDialer struct{ err error }

func (d errorDialer) DialContext(_ context.Context, _, _ string) (net.Conn, error) {
	return nil, d.err
}

func TestTCPExecutorClassifiesFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "dns", err: &net.DNSError{Err: "not found", Name: "bad.invalid"}, want: "dns_failed"},
		{name: "timeout", err: context.DeadlineExceeded, want: "connect_timeout"},
		{name: "refused", err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, want: "connect_refused"},
		{name: "generic", err: syscall.ENETUNREACH, want: "connect_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := (TCPExecutor{Dialer: errorDialer{err: tc.err}}).executeProbe(t.Context(), tcpProbe("example.com", 443))
			if got := classifyTCPConnect(r.ConnectErr); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTCPExecutorRejectsInvalidTarget(t *testing.T) {
	r := (TCPExecutor{}).executeProbe(t.Context(), tcpProbe("", 0))
	if !r.ConfigError {
		t.Fatalf("expected config error, got %+v", r)
	}
}
