package probes

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// DNSResult holds the outcome of a single DNS probe execution.
type DNSResult struct {
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
func (r DNSResult) Success() bool { return !r.ConfigError && r.ResolverErr == nil }

// DNSExecutor runs DNS probes using github.com/miekg/dns.
type DNSExecutor struct{}

// Execute performs a DNS query for the given probe and returns the result.
func (e DNSExecutor) Execute(ctx context.Context, probe *syntheticsv1alpha1.DNSProbe) DNSResult {
	start := time.Now()

	queryName := probe.Spec.Query.Name
	if strings.TrimSpace(queryName) == "" {
		return DNSResult{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     "query name must be non-empty",
		}
	}

	queryType := strings.ToUpper(probe.Spec.Query.Type)
	if queryType == "" {
		queryType = "A"
	}
	dnsType, ok := dns.StringToType[queryType]
	if !ok {
		return DNSResult{
			ConfigError: true,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     "unsupported query type: " + queryType,
		}
	}

	resolver := probe.Spec.Query.Resolver
	if resolver == "" {
		resolver = systemResolver()
	}

	host, port, err := net.SplitHostPort(resolver)
	if err != nil || host == "" || port == "" {
		return DNSResult{
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
			return DNSResult{
				Completed: time.Now(),
				Duration:  time.Since(start),
				Message:   "context deadline exceeded before query",
			}
		}
		client.Timeout = remaining
	}

	resp, rtt, err := client.ExchangeContext(ctx, msg, resolver)
	if err != nil {
		return DNSResult{
			ResolverErr: err,
			Completed:   time.Now(),
			Duration:    time.Since(start),
			Message:     err.Error(),
		}
	}

	result := DNSResult{
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
