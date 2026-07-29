package web

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sync"
	"time"
)

// §12.3: Wasi checks its own device-listener certificate and shows a
// persistent guardian-UI banner at under 45 days remaining.
//
// Deliberately not an INBOX notice letter: certificate operations are operator
// noise, not family record, and the child's inbox is not an ops channel. The
// optional SMTP copy to guardian addresses (§7.5) is the other half of the
// alarm and belongs to whoever owns that path, not to a web page.
const certWarnWindow = 45 * 24 * time.Hour

// certCheckInterval is how often the certificate file is re-read. §12.3 asks
// for startup and daily; an hour is close enough to daily to be within the
// spirit, and cheap enough that a renewal shows up in the UI while the
// operator is still at the keyboard rather than the next morning.
const certCheckInterval = time.Hour

// certState caches the parsed expiry so that rendering any page does not mean
// reading and parsing a PEM file.
type certState struct {
	mu        sync.Mutex
	checkedAt time.Time
	warning   string
}

// certWarning returns the banner text, or "" when the certificate has more
// than certWarnWindow left.
//
// A certificate that cannot be read or parsed produces a banner too. The
// alternative — staying silent — turns a misconfigured path into an invisible
// problem that surfaces as a device that has quietly stopped reaching home,
// which is the §12.2 failure this alarm exists to prevent.
func (s *Server) certWarning() string {
	cfg := s.watcher.Current()
	if cfg == nil || cfg.Device.TLSCert == "" {
		return ""
	}

	s.cert.mu.Lock()
	defer s.cert.mu.Unlock()

	now := s.now()
	if !s.cert.checkedAt.IsZero() && now.Sub(s.cert.checkedAt) < certCheckInterval {
		return s.cert.warning
	}
	s.cert.checkedAt = now
	s.cert.warning = certWarningFor(cfg.Device.TLSCert, now)
	return s.cert.warning
}

func certWarningFor(path string, now time.Time) string {
	notAfter, err := certNotAfter(path)
	if err != nil {
		return "The device connection certificate could not be read (" + path + "). Until it is, there is no warning before it expires."
	}

	remaining := notAfter.Sub(now)
	switch {
	case remaining <= 0:
		return fmt.Sprintf("The device connection certificate expired on %s. The device cannot reach home until it is renewed.",
			notAfter.Local().Format("2 January 2006"))
	case remaining < certWarnWindow:
		return fmt.Sprintf("The device connection certificate expires in %s, on %s. Renewing it is a manual step.",
			plural(int(remaining.Hours()/24), "day"), notAfter.Local().Format("2 January 2006"))
	default:
		return ""
	}
}

// certNotAfter reads the leaf certificate's expiry from a PEM file. The first
// CERTIFICATE block is the leaf by convention, and by the same convention any
// blocks after it are the chain.
func certNotAfter(path string) (time.Time, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}, err
	}
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, err
		}
		return leaf.NotAfter, nil
	}
	return time.Time{}, fmt.Errorf("web: no certificate found in %s", path)
}
