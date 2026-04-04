package webhookcerts

import (
	"crypto/x509"
	"encoding/pem"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGenerateBundleProducesServiceDNSNames(t *testing.T) {
	manager := NewManager(logr.Discard(), fake.NewSimpleClientset(), "default", "synthetics-webhook-certs", "synthetics-operator-webhook-service", t.TempDir(), "", "")

	bundle, err := manager.generateBundle()
	if err != nil {
		t.Fatalf("generate bundle: %v", err)
	}

	block, _ := pem.Decode(bundle.ServingCertPEM)
	if block == nil {
		t.Fatal("expected PEM certificate block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	want := "synthetics-operator-webhook-service.default.svc"
	if !slices.Contains(cert.DNSNames, want) {
		t.Fatalf("expected DNS SAN %q, got %v", want, cert.DNSNames)
	}
}

func TestShouldRotate(t *testing.T) {
	if !shouldRotate(&certBundle{NotAfter: time.Now().Add(time.Hour)}, time.Now(), 2*time.Hour) {
		t.Fatal("expected rotation when certificate is inside rotation window")
	}
	if shouldRotate(&certBundle{NotAfter: time.Now().Add(48 * time.Hour)}, time.Now(), 2*time.Hour) {
		t.Fatal("did not expect rotation for healthy certificate")
	}
}
