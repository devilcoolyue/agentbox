package gitaccess

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
)

// NetworkPolicy is immutable with an existing connection. A changed trust root
// or route requires a new connection/application, avoiding silent credential
// redirection. Empty route preserves direct access for older installations.
type NetworkPolicy struct {
	Route string `json:"route,omitempty"`
	CAPEM string `json:"ca_pem,omitempty"`
}

func (p NetworkPolicy) Validate() error {
	if p.Route != "" && p.Route != "direct" && p.Route != "tunnel" {
		return errors.New("Git 网络路由只能选择 direct 或 tunnel")
	}
	if len(p.CAPEM) > 32<<10 {
		return errors.New("公司 CA 证书不能超过 32 KiB")
	}
	if p.CAPEM != "" {
		_, err := p.roots()
		return err
	}
	return nil
}
func (p NetworkPolicy) roots() (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	rest := []byte(p.CAPEM)
	n := 0
	for len(bytes.TrimSpace(rest)) > 0 {
		rest = bytes.TrimSpace(rest)
		if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("CA 只能包含 PEM 证书，不能包含私钥或其他文本")
		}
		block, next := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) > 0 {
			return nil, errors.New("CA PEM 证书格式无效")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || !cert.BasicConstraintsValid {
			return nil, errors.New("请填写有效的公司根 CA 或中间 CA 证书")
		}
		roots.AddCert(cert)
		rest = next
		n++
	}
	if n == 0 {
		return nil, errors.New("CA PEM 证书为空")
	}
	return roots, nil
}
func (p NetworkPolicy) TLSConfig() (*tls.Config, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.CAPEM != "" {
		var err error
		cfg.RootCAs, err = p.roots()
		if err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// Identity is added to AEAD associated data only for configured policies. Old
// rows with no policy keep their existing associated data and remain readable.
func (p NetworkPolicy) Identity() string {
	raw, _ := json.Marshal(p)
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}
func (p NetworkPolicy) Configured() bool { return p.Route != "" || p.CAPEM != "" }
