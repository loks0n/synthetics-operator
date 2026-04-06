package webhookcerts

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const (
	DefaultCertName = "tls.crt"
	DefaultKeyName  = "tls.key"
	DefaultCAName   = "ca.crt"
	DefaultCAKey    = "ca.key"
)

type Manager struct {
	logger                         logr.Logger
	clientset                      kubernetes.Interface
	namespace                      string
	secretName                     string
	serviceName                    string
	validatingWebhookConfiguration string
	mutatingWebhookConfiguration   string
	certTTL                        time.Duration
	rotateBefore                   time.Duration
	checkInterval                  time.Duration
	currentCert                    atomic.Pointer[tls.Certificate]
}

func NewManager(
	logger logr.Logger,
	clientset kubernetes.Interface,
	namespace, secretName, serviceName, _, validatingWebhookConfiguration, mutatingWebhookConfiguration string,
) *Manager {
	return &Manager{
		logger:                         logger,
		clientset:                      clientset,
		namespace:                      namespace,
		secretName:                     secretName,
		serviceName:                    serviceName,
		validatingWebhookConfiguration: validatingWebhookConfiguration,
		mutatingWebhookConfiguration:   mutatingWebhookConfiguration,
		certTTL:                        90 * 24 * time.Hour,
		rotateBefore:                   18 * 24 * time.Hour,
		checkInterval:                  time.Hour,
	}
}

func (m *Manager) Initialize(ctx context.Context) error {
	bundle, err := m.ensureSecret(ctx)
	if err != nil {
		return err
	}
	if err := m.loadBundle(bundle); err != nil {
		return err
	}
	return m.injectCABundle(ctx, bundle.CACertPEM)
}

func (m *Manager) Start(ctx context.Context) error {
	if err := m.startSecretInformer(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			bundle, err := m.ensureSecret(ctx)
			if err != nil {
				m.logger.Error(err, "reconciling webhook certificates")
				continue
			}
			if err := m.loadBundle(bundle); err != nil {
				m.logger.Error(err, "loading rotated webhook certificate")
				continue
			}
			if err := m.injectCABundle(ctx, bundle.CACertPEM); err != nil {
				m.logger.Error(err, "injecting webhook ca bundle")
			}
		}
	}
}

func (m *Manager) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert := m.currentCert.Load()
	if cert == nil {
		return nil, errors.New("webhook serving certificate is not loaded")
	}
	return cert, nil
}

func (m *Manager) startSecretInformer(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		m.clientset,
		0,
		informers.WithNamespace(m.namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.FieldSelector = fields.OneTermEqualSelector("metadata.name", m.secretName).String()
		}),
	)

	informer := factory.Core().V1().Secrets().Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			m.handleSecret(obj)
		},
		UpdateFunc: func(_, newObj any) {
			m.handleSecret(newObj)
		},
	})
	if err != nil {
		return err
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return errors.New("timed out waiting for webhook certificate informer cache sync")
	}
	return nil
}

func (m *Manager) handleSecret(obj any) {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Name != m.secretName || secret.Namespace != m.namespace {
		return
	}
	bundle, err := bundleFromSecret(secret)
	if err != nil {
		m.logger.Error(err, "parsing watched webhook certificate secret", "secret", secret.Name)
		return
	}
	if err := m.loadBundle(bundle); err != nil {
		m.logger.Error(err, "loading watched webhook certificate secret", "secret", secret.Name)
	}
}

type certBundle struct {
	CACertPEM      []byte
	CAKeyPEM       []byte
	ServingCertPEM []byte
	ServingKeyPEM  []byte
	NotAfter       time.Time
}

func (m *Manager) ensureSecret(ctx context.Context) (*certBundle, error) {
	secrets := m.clientset.CoreV1().Secrets(m.namespace)
	secret, err := secrets.Get(ctx, m.secretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		bundle, genErr := m.generateBundle()
		if genErr != nil {
			return nil, genErr
		}
		_, createErr := secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      m.secretName,
				Namespace: m.namespace,
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{
				DefaultCAName:   bundle.CACertPEM,
				DefaultCAKey:    bundle.CAKeyPEM,
				DefaultCertName: bundle.ServingCertPEM,
				DefaultKeyName:  bundle.ServingKeyPEM,
			},
		}, metav1.CreateOptions{})
		if createErr != nil {
			return nil, createErr
		}
		m.logger.Info("created webhook certificate secret", "namespace", m.namespace, "name", m.secretName)
		return bundle, nil
	}
	if err != nil {
		return nil, err
	}

	bundle, err := bundleFromSecret(secret)
	if err != nil || shouldRotate(bundle, time.Now(), m.rotateBefore) {
		newBundle, genErr := m.generateBundle()
		if genErr != nil {
			return nil, genErr
		}
		updatedSecret := secret.DeepCopy()
		if updatedSecret.Data == nil {
			updatedSecret.Data = map[string][]byte{}
		}
		updatedSecret.Data[DefaultCAName] = newBundle.CACertPEM
		updatedSecret.Data[DefaultCAKey] = newBundle.CAKeyPEM
		updatedSecret.Data[DefaultCertName] = newBundle.ServingCertPEM
		updatedSecret.Data[DefaultKeyName] = newBundle.ServingKeyPEM
		if _, updateErr := secrets.Update(ctx, updatedSecret, metav1.UpdateOptions{}); updateErr != nil {
			return nil, updateErr
		}
		m.logger.Info("rotated webhook certificate secret", "namespace", m.namespace, "name", m.secretName)
		return newBundle, nil
	}

	return bundle, nil
}

func (m *Manager) loadBundle(bundle *certBundle) error {
	tlsCert, err := tls.X509KeyPair(bundle.ServingCertPEM, bundle.ServingKeyPEM)
	if err != nil {
		return err
	}
	m.currentCert.Store(&tlsCert)
	return nil
}

func (m *Manager) injectCABundle(ctx context.Context, caBundle []byte) error {
	if m.validatingWebhookConfiguration != "" {
		if err := patchValidatingWebhookConfiguration(ctx, m.clientset, m.validatingWebhookConfiguration, caBundle); err != nil {
			return err
		}
	}
	if m.mutatingWebhookConfiguration != "" {
		if err := patchMutatingWebhookConfiguration(ctx, m.clientset, m.mutatingWebhookConfiguration, caBundle); err != nil {
			return err
		}
	}
	return nil
}

func patchValidatingWebhookConfiguration(ctx context.Context, clientset kubernetes.Interface, name string, caBundle []byte) error {
	client := clientset.AdmissionregistrationV1().ValidatingWebhookConfigurations()
	current, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	updatedWebhookConfig := current.DeepCopy()
	changed := false
	for i := range updatedWebhookConfig.Webhooks {
		if string(updatedWebhookConfig.Webhooks[i].ClientConfig.CABundle) != string(caBundle) {
			updatedWebhookConfig.Webhooks[i].ClientConfig.CABundle = caBundle
			changed = true
		}
	}
	if !changed {
		return nil
	}
	_, err = client.Update(ctx, updatedWebhookConfig, metav1.UpdateOptions{})
	return err
}

func patchMutatingWebhookConfiguration(ctx context.Context, clientset kubernetes.Interface, name string, caBundle []byte) error {
	client := clientset.AdmissionregistrationV1().MutatingWebhookConfigurations()
	current, err := client.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	updatedWebhookConfig := current.DeepCopy()
	changed := false
	for i := range updatedWebhookConfig.Webhooks {
		if string(updatedWebhookConfig.Webhooks[i].ClientConfig.CABundle) != string(caBundle) {
			updatedWebhookConfig.Webhooks[i].ClientConfig.CABundle = caBundle
			changed = true
		}
	}
	if !changed {
		return nil
	}
	_, err = client.Update(ctx, updatedWebhookConfig, metav1.UpdateOptions{})
	return err
}

func (m *Manager) generateBundle() (*certBundle, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName:   "synthetics-operator-webhook-ca",
			Organization: []string{"synthetics-operator"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(m.certTTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	servingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	servingTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1),
		Subject: pkix.Name{
			CommonName: fmt.Sprintf("%s.%s.svc", m.serviceName, m.namespace),
		},
		DNSNames: []string{
			m.serviceName,
			fmt.Sprintf("%s.%s", m.serviceName, m.namespace),
			fmt.Sprintf("%s.%s.svc", m.serviceName, m.namespace),
		},
		NotBefore:   time.Now().Add(-time.Hour),
		NotAfter:    time.Now().Add(m.certTTL),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	servingDER, err := x509.CreateCertificate(rand.Reader, servingTemplate, caTemplate, &servingKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}

	return &certBundle{
		CACertPEM:      pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CAKeyPEM:       pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)}),
		ServingCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: servingDER}),
		ServingKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(servingKey)}),
		NotAfter:       servingTemplate.NotAfter,
	}, nil
}

func bundleFromSecret(secret *corev1.Secret) (*certBundle, error) {
	caCertPEM := secret.Data[DefaultCAName]
	caKeyPEM := secret.Data[DefaultCAKey]
	servingCertPEM := secret.Data[DefaultCertName]
	servingKeyPEM := secret.Data[DefaultKeyName]
	if len(caCertPEM) == 0 || len(caKeyPEM) == 0 || len(servingCertPEM) == 0 || len(servingKeyPEM) == 0 {
		return nil, fmt.Errorf("secret %s/%s is missing certificate data", secret.Namespace, secret.Name)
	}

	block, _ := pem.Decode(servingCertPEM)
	if block == nil {
		return nil, errors.New("failed to decode serving certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}

	return &certBundle{
		CACertPEM:      caCertPEM,
		CAKeyPEM:       caKeyPEM,
		ServingCertPEM: servingCertPEM,
		ServingKeyPEM:  servingKeyPEM,
		NotAfter:       cert.NotAfter,
	}, nil
}

func shouldRotate(bundle *certBundle, now time.Time, rotateBefore time.Duration) bool {
	if bundle == nil {
		return true
	}
	return now.Add(rotateBefore).After(bundle.NotAfter)
}
