package main

// Paired devices for `agentpad serve`. A phone pairs by scanning a QR code
// that holds a short-lived, single-use pairing code; it trades the code for
// a long-lived token. Only the token's SHA-256 is kept, in
// ~/.config/agentpad/devices.json, so the file alone can't be used to
// answer anything. Revoking a device removes it from the file, which the
// server reads on every request.

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"token_sha256"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
	LastAddr  string    `json:"last_addr,omitempty"`
	APNsToken string    `json:"apns_token,omitempty"`
	APNsEnv   string    `json:"apns_env,omitempty"` // sandbox or production
}

func configDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "agentpad")
}

func devicesPath() string { return filepath.Join(configDir(), "devices.json") }
func pairingPath() string { return filepath.Join(stateDir(), "pairing.json") }

var devicesMu sync.Mutex

func loadDevices() []device {
	var out []device
	if data, err := os.ReadFile(devicesPath()); err == nil {
		json.Unmarshal(data, &out)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func saveDevices(list []device) error {
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(list, "", "  ")
	return writeAtomic(devicesPath(), data)
}

// changeDevices edits the device list under the lock.
func changeDevices(change func(list []device) []device) error {
	devicesMu.Lock()
	defer devicesMu.Unlock()
	return saveDevices(change(loadDevices()))
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// deviceFor is the paired device a bearer token belongs to.
func deviceFor(tok string) (device, bool) {
	if tok == "" {
		return device{}, false
	}
	want := hashToken(tok)
	for _, d := range loadDevices() {
		if subtle.ConstantTimeCompare([]byte(d.TokenHash), []byte(want)) == 1 {
			return d, true
		}
	}
	return device{}, false
}

// touchDevice notes when and where a device was last seen, at most once a
// minute so busy streams don't rewrite the file.
func touchDevice(d device, addr string) {
	if time.Since(d.LastSeen) < time.Minute && d.LastAddr == addr {
		return
	}
	changeDevices(func(list []device) []device {
		for i := range list {
			if list[i].ID == d.ID {
				list[i].LastSeen, list[i].LastAddr = time.Now(), addr
			}
		}
		return list
	})
}

type pairing struct {
	CodeHash string    `json:"code_sha256"`
	Expires  time.Time `json:"expires"`
}

const pairingTTL = 5 * time.Minute

// newPairing makes a fresh pairing code and stores its hash, replacing any
// earlier one.
func newPairing() (string, time.Time, error) {
	code := randomToken(18)
	p := pairing{CodeHash: hashToken(code), Expires: time.Now().Add(pairingTTL)}
	if err := os.MkdirAll(stateDir(), 0o700); err != nil {
		return "", time.Time{}, err
	}
	data, _ := json.Marshal(p)
	return code, p.Expires, writeAtomic(pairingPath(), data)
}

var errBadPairing = errors.New("that pairing code isn't valid: run agentpad serve pair again")

// redeemPairing checks code against the open pairing and, when it matches,
// uses it up and pairs a new device. It returns the device and its token.
func redeemPairing(code, name, addr string) (device, string, error) {
	devicesMu.Lock()
	defer devicesMu.Unlock()
	data, err := os.ReadFile(pairingPath())
	if err != nil {
		return device{}, "", errBadPairing
	}
	var p pairing
	if json.Unmarshal(data, &p) != nil || time.Now().After(p.Expires) ||
		subtle.ConstantTimeCompare([]byte(p.CodeHash), []byte(hashToken(code))) != 1 {
		return device{}, "", errBadPairing
	}
	os.Remove(pairingPath()) // single use
	name = strings.TrimSpace(strings.ReplaceAll(noControl(name), "\n", " "))
	if name == "" {
		name = "iPhone"
	}
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}
	tok := randomToken(32)
	d := device{ID: newID(), Name: name, TokenHash: hashToken(tok), Created: time.Now(), LastSeen: time.Now(), LastAddr: addr}
	list := append(loadDevices(), d)
	if err := saveDevices(list); err != nil {
		return device{}, "", err
	}
	return d, tok, nil
}

func revokeDevice(id string) bool {
	found := false
	changeDevices(func(list []device) []device {
		out := list[:0]
		for _, d := range list {
			if d.ID == id || len(id) >= 4 && len(d.ID) >= len(id) && d.ID[:len(id)] == id {
				found = true
				continue
			}
			out = append(out, d)
		}
		return out
	})
	return found
}
