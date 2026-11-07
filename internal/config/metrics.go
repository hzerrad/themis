package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Metrics says where Themis reads how much memory and CPU containers use.
// Prometheus is asked first, when it has an address; the Kubernetes metrics
// API answers what Prometheus can't.
type Metrics struct {
	// Kubernetes is the Kubernetes metrics API (metrics.k8s.io), which
	// metrics-server serves: what containers use now, with no history. On
	// unless disabled; where the cluster doesn't serve it, the status page
	// says so.
	Kubernetes MetricsAPI `json:"kubernetes,omitzero"`

	// Prometheus is a server answering Prometheus queries: Prometheus,
	// Thanos, Mimir, VictoriaMetrics or Grafana Cloud. It has history, so
	// it also answers peak memory, CPU throttling, and how an earlier
	// revision's pods ran and how much of their limits they used.
	Prometheus *Prometheus `json:"prometheus,omitempty"`
}

// MetricsAPI configures reading the Kubernetes metrics API.
type MetricsAPI struct {
	Disabled bool `json:"disabled,omitempty"`
}

// Prometheus configures reading a server that answers Prometheus queries.
type Prometheus struct {
	// Address is the server's base URL, under which /api/v1/query is
	// served, for example http://prometheus-operated.monitoring:9090.
	Address string `json:"address" jsonschema:"minLength=1"`

	// BearerTokenFile holds a token sent as a bearer token, if the server
	// needs one. Read on every query, so rotation needs no restart.
	BearerTokenFile string `json:"bearerTokenFile,omitempty"`

	// TLS configures an https address.
	TLS TLS `json:"tls,omitzero"`
}

// TLS configures how Themis checks a server, and proves who it is to one.
type TLS struct {
	// CAFile holds the certificates that sign the server's, when the system's
	// don't.
	CAFile string `json:"caFile,omitempty"`
	// CertFile and KeyFile are a client certificate, for a server that asks
	// for one. Set both or neither.
	CertFile string `json:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty"`
	// InsecureSkipVerify accepts any certificate. Only for trying Themis.
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`
}

// ClientConfig is how Themis connects to the server. The CA is read now; the
// client certificate on every handshake, so a renewed one needs no restart.
func (t TLS) ClientConfig() (*tls.Config, error) {
	//nolint:gosec // InsecureSkipVerify is the operator's explicit choice
	out := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: t.InsecureSkipVerify}
	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, err
		}
		out.RootCAs = x509.NewCertPool()
		if !out.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s holds no PEM certificate", t.CAFile)
		}
	}
	if t.CertFile != "" {
		out.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
			return &cert, err
		}
	}
	return out, nil
}

func (m Metrics) validate(add func(path, format string, args ...any)) {
	p := m.Prometheus
	if p == nil {
		return
	}
	if !IsHTTPURL(p.Address) {
		add("metrics.prometheus.address", "%q is not an http or https URL", p.Address)
	}
	if (p.TLS.CertFile == "") != (p.TLS.KeyFile == "") {
		add("metrics.prometheus.tls", "set both certFile and keyFile, or neither")
	}
}
