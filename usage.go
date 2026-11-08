package main

// Usage: how full each agent's context is, and how much of the plan is
// left. Claude Code hands both to its status line command on every refresh,
// so `agentpad statusline` sits in front of yours: it notes the numbers and
// runs your command with the same input, so your status line looks as it did.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// contextUse is how full an agent's context window is.
type contextUse struct {
	Tokens int `json:"tokens"`
	Window int `json:"window"`
}

func (c contextUse) frac() float64 {
	if c.Window <= 0 {
		return 0
	}
	return min(1, float64(c.Tokens)/float64(c.Window))
}

// planWindow is one of the plan's usage windows: the 5-hour or the weekly.
type planWindow struct {
	Used    float64   `json:"used"` // percent, 0-100
	ResetAt time.Time `json:"reset_at"`
}

type planUsage struct {
	FiveHour *planWindow `json:"five_hour,omitempty"`
	Weekly   *planWindow `json:"weekly,omitempty"`
	Updated  time.Time   `json:"updated"`
}

func usageDir() string { return filepath.Join(stateDir(), "usage") }
func planPath() string { return filepath.Join(stateDir(), "plan.json") }

// runStatusline is `agentpad statusline [COMMAND...]`.
func runStatusline(args []string) error {
	in, _ := io.ReadAll(os.Stdin)
	noteStatus(in)
	if len(args) == 0 {
		return nil
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// noteStatus keeps what a status line input says about usage. Anything it
// can't read is left out; the status line still runs.
func noteStatus(in []byte) {
	var st struct {
		SessionID     string `json:"session_id"`
		ContextWindow struct {
			Size         int     `json:"context_window_size"`
			UsedPercent  float64 `json:"used_percentage"`
			InputTokens  int     `json:"total_input_tokens"`
			CurrentUsage *struct {
				Input       int `json:"input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
			} `json:"current_usage"`
		} `json:"context_window"`
		RateLimits map[string]struct {
			Used    float64         `json:"used_percentage"`
			ResetAt json.RawMessage `json:"resets_at"`
		} `json:"rate_limits"`
	}
	if json.Unmarshal(in, &st) != nil || st.SessionID == "" {
		return
	}
	if cw := st.ContextWindow; cw.Size > 0 {
		c := contextUse{Window: cw.Size}
		switch {
		case cw.CurrentUsage != nil:
			c.Tokens = cw.CurrentUsage.Input + cw.CurrentUsage.CacheCreate + cw.CurrentUsage.CacheRead
		case cw.UsedPercent > 0:
			c.Tokens = int(cw.UsedPercent / 100 * float64(cw.Size))
		}
		if data, err := json.Marshal(c); err == nil && os.MkdirAll(usageDir(), 0o700) == nil {
			writeAtomic(filepath.Join(usageDir(), safeName(st.SessionID)+".json"), data)
		}
	}
	if len(st.RateLimits) > 0 {
		p := planUsage{Updated: time.Now()}
		for name, w := range st.RateLimits {
			pw := &planWindow{Used: w.Used, ResetAt: parseWhen(w.ResetAt)}
			switch name {
			case "five_hour":
				p.FiveHour = pw
			case "seven_day":
				p.Weekly = pw
			}
		}
		if data, err := json.Marshal(p); err == nil {
			writeAtomic(planPath(), data)
		}
	}
}

// parseWhen reads a time given as Unix seconds or as RFC 3339.
func parseWhen(raw json.RawMessage) time.Time {
	s := strings.Trim(string(raw), `"`)
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		if n > 1e12 { // milliseconds
			n /= 1000
		}
		return time.Unix(int64(n), 0)
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func loadPlan() (planUsage, bool) {
	var p planUsage
	data, err := os.ReadFile(planPath())
	if err != nil || json.Unmarshal(data, &p) != nil {
		return p, false
	}
	return p, p.FiveHour != nil || p.Weekly != nil
}

var ctxCache = struct {
	sync.Mutex
	m map[string]ctxEntry
}{m: map[string]ctxEntry{}}

type ctxEntry struct {
	stamp time.Time
	use   contextUse
	ok    bool
}

// contextOf is how full a's context is: from the status line when it has
// reported, else from the transcript's last reply.
func contextOf(a Agent) (contextUse, bool) {
	if data, err := os.ReadFile(filepath.Join(usageDir(), safeName(a.SessionID)+".json")); err == nil {
		var c contextUse
		if json.Unmarshal(data, &c) == nil && c.Window > 0 {
			return c, true
		}
	}
	if a.Transcript == "" {
		return contextUse{}, false
	}
	fi, err := os.Stat(a.Transcript)
	if err != nil {
		return contextUse{}, false
	}
	ctxCache.Lock()
	defer ctxCache.Unlock()
	if e, ok := ctxCache.m[a.Transcript]; ok && e.stamp.Equal(fi.ModTime()) {
		return e.use, e.ok
	}
	use, ok := transcriptContext(a.Transcript, fi.Size())
	ctxCache.m[a.Transcript] = ctxEntry{fi.ModTime(), use, ok}
	return use, ok
}

// transcriptContext reads the context size off the last main-thread reply
// in the transcript's tail.
func transcriptContext(path string, size int64) (contextUse, bool) {
	f, err := os.Open(path)
	if err != nil {
		return contextUse{}, false
	}
	defer f.Close()
	const tail = 1 << 20
	if size > tail {
		f.Seek(size-tail, io.SeekStart)
	}
	var last contextUse
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 256<<10), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		var e struct {
			Type      string `json:"type"`
			Sidechain bool   `json:"isSidechain"`
			Message   struct {
				Model string `json:"model"`
				Usage *struct {
					Input       int `json:"input_tokens"`
					CacheCreate int `json:"cache_creation_input_tokens"`
					CacheRead   int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Sidechain || e.Message.Usage == nil {
			continue
		}
		u := e.Message.Usage
		tokens := u.Input + u.CacheCreate + u.CacheRead
		if tokens == 0 {
			continue
		}
		window := 200_000
		if tokens > window || strings.Contains(strings.ToLower(e.Message.Model), "1m") {
			window = 1_000_000
		}
		last, found = contextUse{Tokens: tokens, Window: window}, true
	}
	return last, found
}
