package main

// HTTPS on the Tailscale side. Tailscale hands out real Let's Encrypt
// certificates for a machine's MagicDNS name (machine.tailnet.ts.net), so
// the phone can talk to the bridge over ordinary HTTPS and the app needs no
// exception to iOS's plain-HTTP block. The certificate and key come from
// `tailscale cert` printed to stdout and stay in memory: the key is never
// written to disk by the bridge. Tailscale caches and renews them itself;
// the bridge asks again twice a day.
//
// Localhost stays plain HTTP: it never leaves this machine, and the
// Simulator pairs through it.

import (
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// tailscaleCLI finds the tailscale command: on PATH, or inside the Mac app.
func tailscaleCLI() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/Tailscale.app/Contents/MacOS/Tailscale",
			"/usr/local/bin/tailscale",
			"/opt/homebrew/bin/tailscale",
		} {
			if _, err := exec.LookPath(p); err == nil {
				return p
			}
		}
	}
	return ""
}

// tsCommand runs the tailscale CLI. The Mac app's binary only acts as the
// CLI when TERM is set; without it (as under launchd) it tries to start a
// second copy of the app instead.
func tsCommand(args ...string) *exec.Cmd {
	cmd := exec.Command(tailscaleCLI(), args...)
	if os.Getenv("TERM") == "" {
		cmd.Env = append(os.Environ(), "TERM=dumb")
	}
	return cmd
}

// tailscaleCertDomain is the name Tailscale will issue a certificate for,
// or "" when HTTPS certificates are off for the tailnet.
func tailscaleCertDomain() (string, error) {
	cli := tailscaleCLI()
	if cli == "" {
		return "", fmt.Errorf("no tailscale command found")
	}
	out, err := tsCommand("status", "--json").Output()
	if err != nil {
		return "", fmt.Errorf("tailscale status: %v", err)
	}
	var st struct {
		CertDomains []string
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("tailscale status: %v", err)
	}
	if len(st.CertDomains) == 0 {
		return "", fmt.Errorf("HTTPS certificates are off for this tailnet (turn them on in the Tailscale admin console, DNS page)")
	}
	return st.CertDomains[0], nil
}

type tsCert struct {
	domain string
	mu     sync.RWMutex
	cert   *tls.Certificate
}

// fetch asks Tailscale for the certificate and key, both printed to stdout.
func (c *tsCert) fetch() error {
	cli := tailscaleCLI()
	if cli == "" {
		return fmt.Errorf("no tailscale command found")
	}
	cmd := tsCommand("cert", "--cert-file", "-", "--key-file", "-", c.domain)
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		if runtime.GOOS == "linux" && strings.Contains(msg, "denied") {
			msg += " (let your user fetch certificates: sudo tailscale set --operator=$USER)"
		}
		return fmt.Errorf("tailscale cert: %s", msg)
	}
	var certPEM, keyPEM []byte
	for rest := out; ; {
		var b *pem.Block
		b, rest = pem.Decode(rest)
		if b == nil {
			break
		}
		if b.Type == "CERTIFICATE" {
			certPEM = append(certPEM, pem.EncodeToMemory(b)...)
		} else if strings.HasSuffix(b.Type, "PRIVATE KEY") {
			keyPEM = pem.EncodeToMemory(b)
		}
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("tailscale cert: %v", err)
	}
	c.mu.Lock()
	c.cert = &pair
	c.mu.Unlock()
	return nil
}

func (c *tsCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cert == nil {
		return nil, fmt.Errorf("no certificate yet")
	}
	return c.cert, nil
}

// loadTailscaleCert gets the certificate once and keeps it fresh.
func loadTailscaleCert() (*tsCert, error) {
	domain, err := tailscaleCertDomain()
	if err != nil {
		return nil, err
	}
	c := &tsCert{domain: domain}
	if err := c.fetch(); err != nil {
		return nil, err
	}
	go func() {
		for range time.Tick(12 * time.Hour) {
			if err := c.fetch(); err != nil {
				logf("serve: renew certificate: %v", err)
			}
		}
	}()
	return c, nil
}

func (c *tsCert) config() *tls.Config {
	return &tls.Config{GetCertificate: c.get, MinVersion: tls.VersionTLS12}
}
