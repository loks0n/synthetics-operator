package probes

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/textproto"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/loks0n/synthetics-operator/internal/results"
)

// fakeSMTPServer answers one connection with scripted replies and reports the
// commands it received once the client hangs up.
type fakeSMTPServer struct {
	banner []string
	ehlo   []string
	// certificate enables STARTTLS when advertised in ehlo, and wraps the
	// whole connection when implicit is set.
	certificate *tls.Certificate
	implicit    bool
	silent      bool
}

func (s fakeSMTPServer) start(t *testing.T) (*net.TCPAddr, <-chan []string) {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	received := make(chan []string, 1)
	go func() {
		var commands []string
		defer func() { received <- commands }()
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		if s.silent {
			_, _ = conn.Read(make([]byte, 1))
			return
		}
		if s.implicit {
			conn = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*s.certificate}})
		}
		text := textproto.NewConn(conn)
		writeLines(text, s.banner)
		for {
			line, err := text.ReadLine()
			if err != nil {
				return
			}
			commands = append(commands, line)
			verb := strings.ToUpper(strings.Fields(line)[0])
			switch verb {
			case "EHLO":
				writeLines(text, s.ehlo)
			case "STARTTLS":
				writeLines(text, []string{"220 2.0.0 ready to start TLS"})
				tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{*s.certificate}})
				if tlsConn.HandshakeContext(t.Context()) != nil {
					return
				}
				text = textproto.NewConn(tlsConn)
			case "QUIT":
				writeLines(text, []string{"221 2.0.0 bye"})
				return
			default:
				writeLines(text, []string{"502 5.5.2 command not recognized"})
			}
		}
	}()
	return listener.Addr().(*net.TCPAddr), received
}

func writeLines(text *textproto.Conn, lines []string) {
	for _, line := range lines {
		_ = text.PrintfLine("%s", line)
	}
}

// selfSignedCertificate returns a certificate for 127.0.0.1 valid until
// notAfter, and its PEM encoding for use as a caCert.
func selfSignedCertificate(t *testing.T, notAfter time.Time) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mail.test"},
		NotBefore:             notAfter.Add(-90 * 24 * time.Hour),
		NotAfter:              notAfter,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func smtpJob(address *net.TCPAddr, mode string, tlsConfig *results.TLSConfig, assertions ...results.Assertion) results.ProbeJob {
	return results.ProbeJob{Spec: results.SpecUpdate{
		Kind: results.KindSMTPProbe, Name: "mail", Namespace: "default",
		SMTPProbe: &results.SMTPProbeSpecPayload{
			TimeoutMs: 2000, Host: address.IP.String(), Port: int32(address.Port),
			TLSMode: mode, TLS: tlsConfig, Assertions: assertions,
		},
	}}
}

const readyBanner = "220 mail.test ESMTP ready"

func plainExtensions() []string {
	return []string{"250-mail.test greets probe", "250-PIPELINING", "250 8BITMIME"}
}

func startTLSExtensions() []string {
	return []string{"250-mail.test greets probe", "250-SIZE 35882577", "250 STARTTLS"}
}

func TestSMTPExecutorPlainConversation(t *testing.T) {
	address, received := fakeSMTPServer{
		banner: []string{"220-mail.test ESMTP", "220 multi-line banner"},
		ehlo:   plainExtensions(),
	}.start(t)

	got := SMTPExecutor{Hostname: "probe.test"}.Execute(t.Context(), smtpJob(address, smtpTLSModeNone, nil,
		results.Assertion{Name: "accepted", Expr: "smtp_code = 250"},
		results.Assertion{Name: "no-tls", Expr: "ssl_expiry_days = -1"},
	))
	if got.Result != "ok" || got.SMTPReplyCode != 250 || got.TLSCertExpiryUnix != 0 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.SMTPHost != address.IP.String() || got.SMTPPort != int32(address.Port) || got.SMTPTLSMode != smtpTLSModeNone {
		t.Fatalf("target telemetry not projected: %+v", got)
	}
	if want := []string{"EHLO probe.test", "QUIT"}; !slices.Equal(<-received, want) {
		t.Fatalf("server did not see %v", want)
	}
}

func TestSMTPExecutorRejectsServiceRefusal(t *testing.T) {
	cases := []struct {
		name string
		ehlo []string
		// banner is the first reply; a 421 one is how a server that accepts TCP
		// says it will not serve.
		banner []string
		code   int
	}{
		{name: "421 banner", banner: []string{"421 4.3.2 Service not available"}, code: 421},
		{name: "554 banner", banner: []string{"554 5.7.1 no SMTP service here"}, code: 554},
		{name: "EHLO refused", banner: []string{readyBanner}, ehlo: []string{"550 5.7.1 go away"}, code: 550},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			address, _ := fakeSMTPServer{banner: tc.banner, ehlo: tc.ehlo}.start(t)
			got := SMTPExecutor{}.Execute(t.Context(), smtpJob(address, smtpTLSModeNone, nil))
			if got.Result != "smtp_rejected" || got.SMTPReplyCode != tc.code {
				t.Fatalf("result=%q code=%d, want smtp_rejected %d", got.Result, got.SMTPReplyCode, tc.code)
			}
		})
	}
}

func TestSMTPExecutorStartTLS(t *testing.T) {
	valid, validPEM := selfSignedCertificate(t, time.Now().Add(30*24*time.Hour))
	expired, expiredPEM := selfSignedCertificate(t, time.Now().Add(-time.Hour))

	cases := []struct {
		name        string
		server      fakeSMTPServer
		tlsConfig   *results.TLSConfig
		want        string
		wantExpiry  bool
		wantCommand []string
	}{
		{
			name:        "trusted certificate",
			server:      fakeSMTPServer{banner: []string{readyBanner}, ehlo: startTLSExtensions(), certificate: &valid},
			tlsConfig:   &results.TLSConfig{CACert: validPEM},
			want:        "ok",
			wantExpiry:  true,
			wantCommand: []string{"EHLO probe.test", "STARTTLS", "EHLO probe.test", "QUIT"},
		},
		{
			name:   "STARTTLS not advertised",
			server: fakeSMTPServer{banner: []string{readyBanner}, ehlo: plainExtensions(), certificate: &valid},
			want:   "tls_failed",
			// The probe never sends mail in plaintext; it gives up after EHLO.
			wantCommand: []string{"EHLO probe.test"},
		},
		{
			name:   "untrusted self-signed certificate",
			server: fakeSMTPServer{banner: []string{readyBanner}, ehlo: startTLSExtensions(), certificate: &valid},
			want:   "tls_failed",
		},
		{
			name:      "expired certificate even when trusted",
			server:    fakeSMTPServer{banner: []string{readyBanner}, ehlo: startTLSExtensions(), certificate: &expired},
			tlsConfig: &results.TLSConfig{CACert: expiredPEM},
			want:      "tls_failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			address, received := tc.server.start(t)
			got := SMTPExecutor{Hostname: "probe.test"}.Execute(t.Context(), smtpJob(address, smtpTLSModeStartTLS, tc.tlsConfig,
				results.Assertion{Name: "cert", Expr: "ssl_expiry_days > 29"},
			))
			if got.Result != tc.want {
				t.Fatalf("result=%q, want %q (%+v)", got.Result, tc.want, got)
			}
			if tc.wantExpiry && got.TLSCertExpiryUnix == 0 {
				t.Fatal("certificate expiry not reported")
			}
			commands := <-received
			if tc.wantCommand != nil && !slices.Equal(commands, tc.wantCommand) {
				t.Fatalf("server saw %v, want %v", commands, tc.wantCommand)
			}
		})
	}
}

func TestSMTPExecutorImplicitTLS(t *testing.T) {
	certificate, certificatePEM := selfSignedCertificate(t, time.Now().Add(10*24*time.Hour))
	address, _ := fakeSMTPServer{banner: []string{readyBanner}, ehlo: plainExtensions(), certificate: &certificate, implicit: true}.start(t)

	got := SMTPExecutor{}.Execute(t.Context(), smtpJob(address, smtpTLSModeImplicit, &results.TLSConfig{CACert: certificatePEM},
		results.Assertion{Name: "cert-renewed", Expr: "ssl_expiry_days > 14"},
	))
	if got.Result != "assertion_failed" || got.FailedAssertion != "cert-renewed" {
		t.Fatalf("expected the 10-day certificate to fail the 14-day assertion, got %+v", got)
	}
	if got.SMTPReplyCode != 250 || got.TLSCertExpiryUnix == 0 {
		t.Fatalf("telemetry missing over implicit TLS: %+v", got)
	}
}

func TestSMTPExecutorImplicitTLSAgainstPlaintextServer(t *testing.T) {
	address, _ := fakeSMTPServer{banner: []string{readyBanner}, ehlo: plainExtensions()}.start(t)
	got := SMTPExecutor{}.Execute(t.Context(), smtpJob(address, smtpTLSModeImplicit, &results.TLSConfig{InsecureSkipVerify: true}))
	if got.Result != "tls_failed" {
		t.Fatalf("result=%q, want tls_failed", got.Result)
	}
}

func TestSMTPExecutorSilentServerTimesOut(t *testing.T) {
	address, _ := fakeSMTPServer{silent: true}.start(t)
	job := smtpJob(address, smtpTLSModeNone, nil)
	job.Spec.SMTPProbe.TimeoutMs = 200

	start := time.Now()
	got := SMTPExecutor{}.Execute(t.Context(), job)
	if got.Result != "recv_timeout" || got.SMTPReplyCode != 0 {
		t.Fatalf("result=%q code=%d, want recv_timeout with no reply", got.Result, got.SMTPReplyCode)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("probe ignored its timeout: %s", elapsed)
	}
}

func TestSMTPExecutorRejectsUnknownTLSMode(t *testing.T) {
	address, _ := fakeSMTPServer{banner: []string{readyBanner}, ehlo: plainExtensions()}.start(t)
	if got := (SMTPExecutor{}).Execute(t.Context(), smtpJob(address, "ssl", nil)); got.Result != "config_error" {
		t.Fatalf("result=%q, want config_error", got.Result)
	}
}
