package probes

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/loks0n/synthetics-operator/internal/results"
)

const (
	smtpTLSModeNone     = "none"
	smtpTLSModeStartTLS = "starttls"
	smtpTLSModeImplicit = "implicit"
)

const (
	// smtpMaxReplyLine is the longest reply line a server may send, CRLF
	// included (RFC 5321 §4.5.3.1.5). Reading stops at the first longer one.
	smtpMaxReplyLine = 512
	// smtpMaxReplyLines caps a multiline reply. RFC 5321 sets no count; the
	// longest reply the probe reads is EHLO, one line per extension, and 128
	// is several times the extensions IANA registers. With the line limit it
	// bounds one reply to 64 KiB.
	smtpMaxReplyLines = 128
)

// errSMTPReplyTooLong is a protocol error, so it classifies as smtp_rejected.
var errSMTPReplyTooLong = textproto.ProtocolError("reply exceeds RFC 5321 limits")

// smtpResult holds the conversation outcome while SMTPExecutor builds the
// public ProbeResult. Failure is empty when every reply was 2xx.
type smtpResult struct {
	Failure    string
	ReplyCode  int
	Duration   time.Duration
	CertExpiry *time.Time
}

// SMTPExecutor reads the banner, sends EHLO, upgrades with STARTTLS or
// implicit TLS when asked, and sends QUIT. It never authenticates or sends
// mail, so a server that accepts the connection but refuses service fails
// the probe where a TCPProbe would pass.
type SMTPExecutor struct {
	Dialer TCPDialer
	// Hostname is announced with EHLO; the prober's hostname when empty.
	Hostname string
}

var _ Executor = SMTPExecutor{}

// Execute owns the full SMTPProbe job-to-result policy.
func (e SMTPExecutor) Execute(ctx context.Context, job results.ProbeJob) results.ProbeResult {
	payload := job.Spec.SMTPProbe
	if payload == nil {
		return configErrorResult(job)
	}

	runCtx, cancel := runContext(ctx, payload.TimeoutMs)
	defer cancel()

	raw := e.converse(runCtx, payload)
	out := probeResult(job)
	out.DurationMs = raw.Duration.Milliseconds()
	out.SMTPHost = payload.Host
	out.SMTPPort = payload.Port
	out.SMTPTLSMode = payload.TLSMode
	out.SMTPReplyCode = raw.ReplyCode
	if raw.CertExpiry != nil {
		out.TLSCertExpiryUnix = raw.CertExpiry.Unix()
	}

	switch {
	case raw.Failure != "":
		out.Result = raw.Failure
	case len(payload.Assertions) > 0:
		out.Result, out.FailedAssertion, out.AssertionResults = evalSMTPAssertions(raw, payload.Assertions)
	default:
		out.Result = "ok"
	}
	return out
}

func (e SMTPExecutor) converse(ctx context.Context, payload *results.SMTPProbeSpecPayload) smtpResult {
	start := time.Now()
	var raw smtpResult
	raw.Failure = e.exchange(ctx, payload, &raw)
	raw.Duration = time.Since(start)
	return raw
}

// exchange runs the conversation, recording the last reply code and the
// certificate expiry on raw, and returns the failure class or "".
func (e SMTPExecutor) exchange(ctx context.Context, payload *results.SMTPProbeSpecPayload, raw *smtpResult) string {
	host := strings.TrimSpace(payload.Host)
	if host == "" || payload.Port < 1 || payload.Port > 65535 {
		return "config_error"
	}
	mode := payload.TLSMode
	if mode != smtpTLSModeNone && mode != smtpTLSModeStartTLS && mode != smtpTLSModeImplicit {
		return "config_error"
	}
	tlsConfig, err := smtpTLSConfig(strings.TrimSuffix(host, "."), payload.TLS)
	if err != nil {
		return "config_error"
	}

	dialer := e.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(payload.Port))))
	if err != nil {
		return classifyTCPConnect(err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if mode == smtpTLSModeImplicit {
		if conn, err = handshakeSMTP(ctx, conn, tlsConfig, raw); err != nil {
			return classifySMTPTLS(err)
		}
	}
	session := newSMTPSession(conn, raw)
	if _, err := session.command(""); err != nil {
		return classifySMTPReply(err)
	}
	extensions, err := session.command("EHLO " + e.hostname())
	if err != nil {
		return classifySMTPReply(err)
	}

	if mode == smtpTLSModeStartTLS {
		var failure string
		if conn, failure = e.startTLS(ctx, conn, &session, extensions, tlsConfig); failure != "" {
			return failure
		}
	}

	// Polite close; the 221 reply is not awaited, so a server slow to say
	// goodbye does not stretch the probe to its timeout.
	_ = session.writer.PrintfLine("QUIT")
	return ""
}

// startTLS upgrades a plain session in place and returns the TLS connection,
// or the failure class.
func (e SMTPExecutor) startTLS(ctx context.Context, conn net.Conn, session *smtpSession, extensions string, config *tls.Config) (net.Conn, string) {
	// A server that stops advertising STARTTLS is a TLS failure, not a
	// rejection: it answered every command, but mail to it would travel in
	// plaintext.
	if !advertises(extensions, "STARTTLS") {
		return conn, "tls_failed"
	}
	if _, err := session.command("STARTTLS"); err != nil {
		return conn, classifySMTPReply(err)
	}
	tlsConn, err := handshakeSMTP(ctx, conn, config, session.raw)
	if err != nil {
		return conn, classifySMTPTLS(err)
	}
	*session = newSMTPSession(tlsConn, session.raw)
	// RFC 3207 discards all state after the upgrade, so the server must greet
	// again over TLS before it is usable.
	if _, err := session.command("EHLO " + e.hostname()); err != nil {
		return tlsConn, classifySMTPReply(err)
	}
	return tlsConn, ""
}

func (e SMTPExecutor) hostname() string {
	if e.Hostname != "" {
		return e.Hostname
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "localhost"
}

// smtpSession sends one command at a time and requires a 2xx reply, keeping
// the last reply code on raw for the smtp_code assertion variable.
type smtpSession struct {
	// reader buffers one maximum-length reply line, so a longer line fills it
	// and fails instead of growing.
	reader *bufio.Reader
	writer *textproto.Writer
	raw    *smtpResult
}

func newSMTPSession(conn net.Conn, raw *smtpResult) smtpSession {
	return smtpSession{
		reader: bufio.NewReaderSize(conn, smtpMaxReplyLine),
		writer: textproto.NewWriter(bufio.NewWriter(conn)),
		raw:    raw,
	}
}

// command sends line, or only reads when line is empty (the banner), and
// returns the reply text.
func (s smtpSession) command(line string) (string, error) {
	if line != "" {
		if err := s.writer.PrintfLine("%s", line); err != nil {
			return "", err
		}
	}
	reply, err := s.readReply()
	if err != nil {
		// A reply cut off at the limit still names its code in the first line.
		if len(reply) >= 3 {
			if code, convErr := strconv.Atoi(string(reply[:3])); convErr == nil {
				s.raw.ReplyCode = code
			}
		}
		return "", err
	}
	code, message, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(reply))).ReadResponse(2)
	if code != 0 {
		s.raw.ReplyCode = code
	}
	return message, err
}

// readReply returns the raw lines of one reply, read within the RFC 5321
// limits, for textproto to parse. A line continues the reply while it either
// says so with '-' after the code or does not carry the reply's code, which is
// how textproto groups the lines it reads.
func (s smtpSession) readReply() ([]byte, error) {
	var reply []byte
	for range smtpMaxReplyLines {
		line, err := s.reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return append(reply, line...), errSMTPReplyTooLong
		}
		if err != nil {
			return nil, err
		}
		first := len(reply) == 0
		reply = append(reply, line...)
		continued := len(line) >= 4 && line[3] == '-'
		if first && !continued {
			return reply, nil
		}
		if !first && len(line) >= 4 && line[3] == ' ' && bytes.Equal(line[:3], reply[:3]) {
			return reply, nil
		}
	}
	return reply, errSMTPReplyTooLong
}

// advertises reports whether an EHLO reply lists extension. The first line
// is the server's greeting, not an extension.
func advertises(reply, extension string) bool {
	lines := strings.Split(reply, "\n")
	for _, line := range lines[1:] {
		if fields := strings.Fields(line); len(fields) > 0 && strings.EqualFold(fields[0], extension) {
			return true
		}
	}
	return false
}

func handshakeSMTP(ctx context.Context, conn net.Conn, config *tls.Config, raw *smtpResult) (net.Conn, error) {
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return conn, err
	}
	if certificates := tlsConn.ConnectionState().PeerCertificates; len(certificates) > 0 {
		expiry := certificates[0].NotAfter
		raw.CertExpiry = &expiry
	}
	return tlsConn, nil
}

func smtpTLSConfig(serverName string, config *results.TLSConfig) (*tls.Config, error) {
	out := &tls.Config{ServerName: serverName}
	if config == nil {
		return out, nil
	}
	out.InsecureSkipVerify = config.InsecureSkipVerify
	if config.CACert != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(config.CACert)) {
			return nil, errors.New("tls.caCert contains no valid PEM certificates")
		}
		out.RootCAs = pool
	}
	return out, nil
}

// classifySMTPReply maps a failure after the connection is open to the public
// result vocabulary.
func classifySMTPReply(err error) string {
	var replyErr *textproto.Error
	var protocolErr textproto.ProtocolError
	if errors.As(err, &replyErr) || errors.As(err, &protocolErr) {
		return "smtp_rejected"
	}
	if isTimeout(err) {
		return "recv_timeout"
	}
	return "connect_failed"
}

func classifySMTPTLS(err error) string {
	if isTimeout(err) {
		return "recv_timeout"
	}
	return "tls_failed"
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
