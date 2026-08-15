package probes

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/loks0n/synthetics-operator/internal/results"
)

var _ Executor = DNSExecutor{}

// dnsResult holds resolver details while DNSExecutor builds the public
// ProbeResult returned through the Executor interface.
type dnsResult struct {
	ConfigError      bool
	ResolverErr      error
	Duration         time.Duration
	Completed        time.Time
	Message          string
	FirstAnswerValue string
	FirstAnswerType  string
	AnswerCount      int
	AuthorityCount   int
	AdditionalCount  int
}

// Success reports whether the resolver returned a response. Assertions are
// evaluated separately.
func (r dnsResult) success() bool { return !r.ConfigError && r.ResolverErr == nil }

type dnsQuery struct {
	Name     string
	Type     string
	Resolver string
}

// DNSExecutor runs DNS probes using github.com/miekg/dns.
type DNSExecutor struct{}

// Execute owns the full DNSProbe job-to-result policy.
func (e DNSExecutor) Execute(ctx context.Context, job results.ProbeJob) results.ProbeResult {
	payload := job.Spec.DNSProbe
	if payload == nil {
		return configErrorResult(job)
	}

	runCtx, cancel := runContext(ctx, payload.TimeoutMs)
	defer cancel()

	raw := e.executeQuery(runCtx, dnsQuery{
		Name: payload.Name, Type: payload.Type, Resolver: payload.Resolver,
	})
	out := probeResult(job)
	out.DurationMs = raw.Duration.Milliseconds()
	out.DNSQueryName = payload.Name
	out.DNSResolver = payload.Resolver
	out.DNSFirstAnswerValue = raw.FirstAnswerValue
	out.DNSFirstAnswerType = raw.FirstAnswerType
	out.DNSAnswerCount = raw.AnswerCount
	out.DNSAuthorityCount = raw.AuthorityCount
	out.DNSAdditionalCount = raw.AdditionalCount

	switch {
	case raw.ConfigError:
		out.Result = "config_error"
	case raw.ResolverErr != nil:
		out.Result = "dns_failed"
	case len(payload.Assertions) > 0:
		out.Result, out.FailedAssertion, out.AssertionResults = evalDNSAssertions(raw, payload.Assertions)
	default:
		out.Result = "ok"
	}
	return out
}

func (e DNSExecutor) executeQuery(ctx context.Context, query dnsQuery) dnsResult {
	start := time.Now()

	queryName := query.Name
	if strings.TrimSpace(queryName) == "" {
		return dnsResult{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     "query name must be non-empty",
		}
	}

	queryType := strings.ToUpper(query.Type)
	if queryType == "" {
		queryType = "A"
	}
	dnsType, ok := dns.StringToType[queryType]
	if !ok {
		return dnsResult{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     "unsupported query type: " + queryType,
		}
	}

	resolver := query.Resolver
	if resolver == "" {
		resolver = systemResolver()
	}

	host, port, err := net.SplitHostPort(resolver)
	if err != nil || host == "" || port == "" {
		return dnsResult{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     fmt.Sprintf("invalid resolver address %q: must be host:port", resolver),
		}
	}

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(queryName), dnsType)
	msg.RecursionDesired = true

	client := &dns.Client{}

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return dnsResult{
				Completed: time.Now(),
				Duration:  time.Since(start),
				Message:   "context deadline exceeded before query",
			}
		}
		client.Timeout = remaining
	}

	resp, rtt, err := client.ExchangeContext(ctx, msg, resolver)
	if err != nil {
		return dnsResult{
			ResolverErr: err,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     err.Error(),
		}
	}

	result := dnsResult{
		Completed:       time.Now(),
		Duration:        rtt,
		AnswerCount:     len(resp.Answer),
		AuthorityCount:  len(resp.Ns),
		AdditionalCount: len(resp.Extra),
	}

	if len(resp.Answer) > 0 {
		result.FirstAnswerValue = extractAnswerValue(resp.Answer[0])
		result.FirstAnswerType = dns.TypeToString[resp.Answer[0].Header().Rrtype]
	}

	result.Message = fmt.Sprintf("received %d answer(s)", len(resp.Answer))

	return result
}

func extractAnswerValue(rr dns.RR) string {
	switch r := rr.(type) {
	case *dns.A:
		return r.A.String()
	case *dns.AAAA:
		return r.AAAA.String()
	case *dns.CNAME:
		return strings.TrimSuffix(r.Target, ".")
	case *dns.NS:
		return strings.TrimSuffix(r.Ns, ".")
	case *dns.MX:
		return strings.TrimSuffix(r.Mx, ".")
	case *dns.PTR:
		return strings.TrimSuffix(r.Ptr, ".")
	case *dns.TXT:
		return strings.Join(r.Txt, " ")
	default:
		s := rr.String()
		parts := strings.Fields(s)
		if len(parts) > 4 {
			return strings.Join(parts[4:], " ")
		}
		return s
	}
}

func systemResolver() string { return "8.8.8.8:53" }
