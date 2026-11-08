package main

// Push to the phone when an agent needs you. A pusher is anything that can
// deliver an alert to a device; v1 talks to APNs directly with your own
// .p8 key (token auth over HTTP/2), so no server sits in between. A hosted
// relay can implement the same interface later without the app changing.
//
// What a push says is fixed and deliberately thin: "An agent needs you in
// <project>". Commands, diffs, paths and host names never go in a push, so
// moving to a relay later exposes nothing new. The app fetches the details
// from the bridge when it opens.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type alert struct {
	Title     string
	Body      string
	RequestID string
	Thread    string
}

type pusher interface {
	push(d device, a alert) error
}

// pushGone is returned when APNs says the device token is no longer good.
type pushGone struct{ reason string }

func (e pushGone) Error() string { return "device token gone: " + e.reason }

// apnsConfig is ~/.config/agentpad/apns/apns.json.
type apnsConfig struct {
	KeyID   string `json:"key_id"`
	TeamID  string `json:"team_id"`
	Topic   string `json:"topic"`    // the app's bundle ID
	KeyFile string `json:"key_file"` // defaults to AuthKey_<key_id>.p8 beside it
}

func apnsDir() string {
	if realConfigDir != "" {
		return filepath.Join(realConfigDir, "apns")
	}
	return filepath.Join(configDir(), "apns")
}

type apnsDirect struct {
	cfg    apnsConfig
	key    *ecdsa.PrivateKey
	client *http.Client

	mu      sync.Mutex
	jwt     string
	jwtMade time.Time
}

// loadAPNs reads the APNs key, or says why push is off.
func loadAPNs() (*apnsDirect, error) {
	data, err := os.ReadFile(filepath.Join(apnsDir(), "apns.json"))
	if err != nil {
		return nil, fmt.Errorf("no %s", filepath.Join(apnsDir(), "apns.json"))
	}
	var cfg apnsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("apns.json: %v", err)
	}
	if cfg.KeyID == "" || cfg.TeamID == "" || cfg.Topic == "" {
		return nil, fmt.Errorf("apns.json needs key_id, team_id and topic")
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = "AuthKey_" + cfg.KeyID + ".p8"
	}
	if !filepath.IsAbs(cfg.KeyFile) {
		cfg.KeyFile = filepath.Join(apnsDir(), cfg.KeyFile)
	}
	pemData, err := os.ReadFile(cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, fmt.Errorf("%s isn't a PEM key", cfg.KeyFile)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s isn't an EC key", cfg.KeyFile)
	}
	return &apnsDirect{cfg: cfg, key: ec, client: &http.Client{Timeout: 15 * time.Second}}, nil
}

// token is the provider JWT, remade every 45 minutes (APNs wants it
// between 20 and 60 minutes old).
func (p *apnsDirect) token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jwt != "" && time.Since(p.jwtMade) < 45*time.Minute {
		return p.jwt, nil
	}
	enc := base64.RawURLEncoding
	head, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": p.cfg.KeyID})
	now := time.Now()
	claims, _ := json.Marshal(map[string]any{"iss": p.cfg.TeamID, "iat": now.Unix()})
	unsigned := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	r, s, err := ecdsa.Sign(rand.Reader, p.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	p.jwt, p.jwtMade = unsigned+"."+enc.EncodeToString(sig), now
	return p.jwt, nil
}

func (p *apnsDirect) push(d device, a alert) error {
	if d.APNsToken == "" {
		return nil
	}
	host := "https://api.push.apple.com"
	if d.APNsEnv != "production" {
		host = "https://api.sandbox.push.apple.com"
	}
	payload := map[string]any{
		"aps": map[string]any{
			"alert":     map[string]string{"title": a.Title, "body": a.Body},
			"sound":     "default",
			"thread-id": a.Thread,
			"category":  "NEEDS_YOU",
		},
	}
	if a.RequestID != "" {
		payload["rid"] = a.RequestID
	}
	body, _ := json.Marshal(payload)
	jwt, err := p.token()
	if err != nil {
		return err
	}
	req, _ := http.NewRequest("POST", host+"/3/device/"+d.APNsToken, bytes.NewReader(body))
	req.Header.Set("authorization", "bearer "+jwt)
	req.Header.Set("apns-topic", p.cfg.Topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("apns-expiration", fmt.Sprint(time.Now().Add(30*time.Minute).Unix()))
	if a.RequestID != "" {
		req.Header.Set("apns-collapse-id", a.RequestID)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var why struct {
		Reason string `json:"reason"`
	}
	json.Unmarshal(msg, &why)
	if resp.StatusCode == 410 || why.Reason == "BadDeviceToken" || why.Reason == "Unregistered" {
		return pushGone{why.Reason}
	}
	return fmt.Errorf("apns %d %s", resp.StatusCode, why.Reason)
}
