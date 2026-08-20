// Package certs creates and loads the self-signed certificate that lets the UI
// be reached over HTTPS on a LAN without any external CA.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const validity = 5 * 365 * 24 * time.Hour

// Paths returns the certificate and key file locations inside dir.
func Paths(dir string) (certPath, keyPath string) {
	return filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
}

// EnsureSelfSigned creates a certificate if none exists and returns the paths
// plus the SHA-256 fingerprint operators can compare in the browser warning.
func EnsureSelfSigned(dir string, extraNames []string) (certPath, keyPath, fingerprint string, err error) {
	certPath, keyPath = Paths(dir)

	if data, readErr := os.ReadFile(certPath); readErr == nil {
		if _, statErr := os.Stat(keyPath); statErr == nil {
			fp, fpErr := fingerprintOf(data)
			return certPath, keyPath, fp, fpErr
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", "", err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", "", err
	}

	hostname, _ := os.Hostname()
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: orDefault(hostname, "netcfg"), Organization: []string{"netcfg-web"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(validity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	for _, name := range dnsNames(hostname, extraNames) {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	for _, ip := range localAddresses() {
		template.IPAddresses = append(template.IPAddresses, ip)
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", "", "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return "", "", "", err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", "", "", err
	}

	fp, err := fingerprintOf(certPEM)
	return certPath, keyPath, fp, err
}

func dnsNames(hostname string, extra []string) []string {
	names := []string{"localhost", "127.0.0.1", "::1"}
	if hostname != "" {
		names = append(names, hostname, hostname+".local")
	}
	return append(names, extra...)
}

// localAddresses collects every non-loopback address so the certificate stays
// valid when the device is reached by IP from the LAN.
func localAddresses() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLinkLocalUnicast() {
				out = append(out, ipNet.IP)
			}
		}
	}
	return out
}

func fingerprintOf(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("cannot decode PEM certificate")
	}
	sum := sha256.Sum256(block.Bytes)

	parts := make([]string, 0, len(sum))
	for _, b := range sum {
		parts = append(parts, strings.ToUpper(hex.EncodeToString([]byte{b})))
	}
	return fmt.Sprintf("SHA256:%s", strings.Join(parts, ":")), nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
