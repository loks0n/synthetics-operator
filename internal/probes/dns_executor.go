package probes

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/types"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
)

// DNSResult holds the outcome of a single DNS probe execution.
type DNSResult struct {
	Success          bool
	ConfigError      bool
	Duration         time.Duration
	Completed        time.Time
	Message          string
	FirstAnswerValue string
	FirstAnswerType  string
	AnswerCount      int
	AuthorityCount   int
	AdditionalCount  int
}

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

	// Validate resolver format — net.SplitHostPort accepts host:port only.
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

	// Respect context deadline.
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
			Completed: time.Now(),
			Duration:  time.Since(start),
			Message:   err.Error(),
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

	result.Success = true
	result.Message = fmt.Sprintf("received %d answer(s)", len(resp.Answer))

	return result
}

// extractAnswerValue returns a string representation of the first RR's data.
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
		// Fallback: strip the header and return the remainder.
		s := rr.String()
		parts := strings.Fields(s)
		if len(parts) > 4 {
			return strings.Join(parts[4:], " ")
		}
		return s
	}
}

// systemResolver returns 8.8.8.8:53 as the fallback DNS resolver.
func systemResolver() string {
	return "8.8.8.8:53"
}

// dnsToprobeState converts a DNSResult into a ProbeState for the metrics store.
func dnsToprobeState(r DNSResult) internalmetrics.ProbeState {
	return internalmetrics.ProbeState{
		Kind:                 "DNSProbe",
		Success:              boolToFloat(r.Success && !r.ConfigError),
		DurationMilliseconds: float64(r.Duration.Milliseconds()),
		LastRunTimestamp:     float64(r.Completed.Unix()),
		ConfigError:          boolToFloat(r.ConfigError),
		DNSFirstAnswerValue:  r.FirstAnswerValue,
		DNSFirstAnswerType:   r.FirstAnswerType,
		DNSAnswerCount:       float64(r.AnswerCount),
		DNSAuthorityCount:    float64(r.AuthorityCount),
		DNSAdditionalCount:   float64(r.AdditionalCount),
	}
}

// NewDNSJob constructs a Job for a DNSProbe. This is the only place in the
// codebase that couples the Job abstraction to the DNSProbe CRD type.
func NewDNSJob(probe *syntheticsv1alpha1.DNSProbe, exec DNSExecutor, store *internalmetrics.Store) Job {
	snapshot := probe.DeepCopy()
	key := types.NamespacedName{Namespace: probe.Namespace, Name: probe.Name}
	return Job{
		Key:      key,
		Interval: snapshot.Spec.Interval.Duration,
		Timeout:  snapshot.Spec.Timeout.Duration,
		Run: func(ctx context.Context) {
			r := exec.Execute(ctx, snapshot)
			state := dnsToprobeState(r)
			if len(snapshot.Spec.Assertions) > 0 {
				ok := !r.ConfigError
				if !ok {
					state.FailureReason = ReasonConfigError
				} else {
					ac := assertionContext{
						"answer_count": float64(r.AnswerCount),
						"duration_ms":  float64(r.Duration.Milliseconds()),
					}
					ok, state.FailureReason, state.AssertionResults = evalAssertions(snapshot.Spec.Assertions, ac)
				}
				state.Success = boolToFloat(ok)
			} else if r.ConfigError {
				state.FailureReason = ReasonConfigError
			} else if !r.Success {
				state.FailureReason = ReasonConnectionError
			}
			store.Upsert(key, state)
		},
	}
}
