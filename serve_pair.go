package main

// Pairing a phone, and the demo mode for trying the app.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"rsc.io/qr"
)

func runPair(port int, local bool, png string) error {
	ip, secure := "127.0.0.1", false
	if si, ok := runningServeInfo(); ok && !local && si.TLSHost != "" {
		ip, secure = si.TLSHost, true
	} else if !local {
		ips := tailscaleIPs()
		if len(ips) == 0 {
			return fmt.Errorf("no Tailscale address: start Tailscale, or use --local for the Simulator")
		}
		ip = ips[0]
	}
	if !serveRunning() {
		fmt.Println("Note: agentpad serve isn't running. Start it so the phone can finish pairing.")
	}
	code, expires, err := newPairing()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	q := url.Values{}
	q.Set("h", ip)
	q.Set("p", strconv.Itoa(port))
	q.Set("c", code)
	if secure {
		q.Set("s", "1") // HTTPS
	}
	if demoMode {
		host = "studio"
	}
	q.Set("n", strings.Split(host, ".")[0])
	link := "nod://pair?" + q.Encode()
	c, err := qr.Encode(link, qr.M)
	if err != nil {
		return err
	}
	if png != "" {
		c.Scale = 12
		if err := os.WriteFile(png, c.PNG(), 0o600); err != nil {
			return err
		}
		fmt.Println("Wrote the QR code to", png)
	}
	fmt.Println()
	printQR(c)
	fmt.Printf("\nScan this with Nod (Settings → Pair with a Mac). It works once, until %s.\n", expires.Format("15:04"))
	fmt.Println("Or paste this link in Nod:", link)
	fmt.Println("Waiting for the phone… (Ctrl+C to stop)")

	before := map[string]bool{}
	for _, d := range loadDevices() {
		before[d.ID] = true
	}
	for time.Now().Before(expires) {
		time.Sleep(500 * time.Millisecond)
		for _, d := range loadDevices() {
			if !before[d.ID] {
				fmt.Printf("Paired with %s (%s).\n", d.Name, d.ID)
				return nil
			}
		}
	}
	os.Remove(pairingPath())
	return fmt.Errorf("the pairing code expired: run agentpad serve pair again")
}

func runningServeInfo() (serveInfo, bool) {
	var si serveInfo
	data, err := os.ReadFile(serveInfoPath())
	if err != nil || json.Unmarshal(data, &si) != nil || si.PID <= 0 {
		return si, false
	}
	return si, true
}

func serveRunning() bool {
	si, ok := runningServeInfo()
	if !ok {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(si.Port)), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// printQR draws c with half blocks, two modules per character cell, in
// explicit black and white so it scans whatever the terminal's theme.
func printQR(c *qr.Code) {
	const quiet = 2
	black := func(x, y int) bool {
		if x < 0 || y < 0 || x >= c.Size || y >= c.Size {
			return false
		}
		return c.Black(x, y)
	}
	col := func(b bool) int {
		if b {
			return 16
		}
		return 231
	}
	for y := -quiet; y < c.Size+quiet; y += 2 {
		var b strings.Builder
		b.WriteString("  ")
		for x := -quiet; x < c.Size+quiet; x++ {
			fmt.Fprintf(&b, "\x1b[38;5;%dm\x1b[48;5;%dm▀", col(black(x, y)), col(black(x, y+1)))
		}
		b.WriteString("\x1b[0m")
		fmt.Println(b.String())
	}
}

var (
	demoMode      bool
	realConfigDir string
)

// --- demo -------------------------------------------------------------------

// useDemoDirs points the state and config folders at a scratch place, so a
// demo bridge never sees or answers your real sessions.
func useDemoDirs() {
	demoMode = true
	realConfigDir = configDir() // push still uses your real APNs key
	home, _ := os.UserHomeDir()
	base := filepath.Join(home, ".local", "state", "agentpad-demo")
	os.Setenv("XDG_STATE_HOME", filepath.Join(base, "state"))
	os.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "config"))
}

type demoAgent struct {
	agent    Agent
	ctx      contextUse
	turns    []map[string]any
	request  func() approvalRequest // what it asks, if it asks
	reqID    string
	answered time.Time
}

func serveDemoAgents() []*demoAgent {
	now := time.Now()
	dir := func(p string) string { return "/home/demo/projects/" + p }
	bash := func(cmd, desc string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"command": cmd, "description": desc})
		return b
	}
	sugg := func(tool, rule string) json.RawMessage {
		b, _ := json.Marshal([]map[string]any{{"type": "addRules", "behavior": "allow", "destination": "localSettings",
			"rules": []map[string]string{{"toolName": tool, "ruleContent": rule}}}})
		return b
	}
	edit, _ := json.Marshal(map[string]any{
		"file_path":  dir("garden-log") + "/beds/water.go",
		"old_string": "func nextWatering(b Bed) time.Time {\n\treturn b.Last.Add(48 * time.Hour)\n}",
		"new_string": "func nextWatering(b Bed, rain float64) time.Time {\n\tgap := 48 * time.Hour\n\tif rain > 5 {\n\t\tgap += 24 * time.Hour\n\t}\n\treturn b.Last.Add(gap)\n}",
	})
	ask, _ := json.Marshal(map[string]any{"questions": []map[string]any{{
		"question": "Which units should the converter start in?", "header": "Units", "multiSelect": false,
		"options": []map[string]string{
			{"label": "Metric", "description": "Grams and millilitres, with cups as a hint"},
			{"label": "US customary", "description": "Cups and ounces, with grams as a hint"},
			{"label": "Follow the phone", "description": "Whatever the region setting says"},
		}}}})
	turn := func(role, text string) map[string]any {
		var content any = text
		if role == "assistant" {
			content = []map[string]string{{"type": "text", "text": text}}
		}
		return map[string]any{"type": role, "message": map[string]any{"role": role, "content": content}}
	}
	return []*demoAgent{
		{
			agent: Agent{SessionID: "demo-route", Cwd: dir("route-planner"), State: stWaiting,
				Activity: "Wants to run go test", Prompt: "Make the elevation profile skip tunnels",
				Tools: 14, Started: now.Add(-6 * time.Minute), Opened: now.Add(-40 * time.Minute)},
			ctx: contextUse{Tokens: 118_400, Window: 200_000},
			turns: []map[string]any{turn("user", "Make the elevation profile skip tunnels"),
				turn("assistant", "I'll treat tunnel segments as flat in the profile and keep their length. Then I'll run the routing tests.")},
			request: func() approvalRequest {
				return approvalRequest{Tool: "Bash", Cwd: dir("route-planner"), SessionID: "demo-route",
					Input: bash("go test ./routing/... -run Elevation", "Run the elevation tests"), Suggestions: sugg("Bash", "go test:*")}
			},
		},
		{
			agent: Agent{SessionID: "demo-garden", Cwd: dir("garden-log"), State: stWaiting,
				Activity: "Wants to edit water.go", Prompt: "Skip a watering day after heavy rain",
				Tools: 9, Started: now.Add(-3 * time.Minute), Opened: now.Add(-2 * time.Hour),
				Tasks: []agentTask{{"1", "Read the rain gauge data", "done"}, {"2", "Adjust the watering gap", "active"}, {"3", "Add a test for wet weeks", "pending"}}},
			ctx: contextUse{Tokens: 64_900, Window: 200_000},
			turns: []map[string]any{turn("user", "Skip a watering day after heavy rain"),
				turn("assistant", "The gauge reports millimetres per day. I'll add a day to the gap when it's over 5 mm.")},
			request: func() approvalRequest {
				return approvalRequest{Tool: "Edit", Cwd: dir("garden-log"), SessionID: "demo-garden", Input: edit,
					Suggestions: []byte(`[{"type":"setMode","mode":"acceptEdits","destination":"session"}]`)}
			},
		},
		{
			agent: Agent{SessionID: "demo-recipe", Cwd: dir("recipe-box"), State: stWaiting,
				Activity: "Asks you a question", Prompt: "Add a unit converter to the recipe view",
				Tools: 5, Started: now.Add(-90 * time.Second), Opened: now.Add(-15 * time.Minute)},
			ctx:   contextUse{Tokens: 31_200, Window: 200_000},
			turns: []map[string]any{turn("user", "Add a unit converter to the recipe view")},
			request: func() approvalRequest {
				return approvalRequest{Tool: "AskUserQuestion", Cwd: dir("recipe-box"), SessionID: "demo-recipe", Input: ask}
			},
		},
		{
			agent: Agent{SessionID: "demo-weather", Cwd: dir("weather-kit"), State: stWorking,
				Activity: "Editing forecast.swift", Prompt: "Show hourly wind gusts on the forecast card",
				Tools: 22, Subagents: 1, Started: now.Add(-11 * time.Minute), Opened: now.Add(-3 * time.Hour),
				Tasks: []agentTask{{"1", "Fetch gusts from the API", "done"}, {"2", "Draw the gust bars", "active"}, {"3", "Check VoiceOver labels", "pending"}, {"4", "Update the snapshot tests", "pending"}}},
			ctx: contextUse{Tokens: 152_300, Window: 200_000},
			turns: []map[string]any{turn("user", "Show hourly wind gusts on the forecast card"),
				turn("assistant", "The API already returns gusts, so this is mostly drawing. I'll put thin bars under the hourly temperatures.")},
		},
		{
			agent: Agent{SessionID: "demo-trail", Cwd: dir("trail-notes"), State: stIdle,
				Said:   "Done. The map now clusters photos by trailhead, and tapping a cluster opens its gallery.",
				Prompt: "Cluster trail photos on the map", Tools: 31, Started: now.Add(-25 * time.Minute), Opened: now.Add(-5 * time.Hour)},
			ctx: contextUse{Tokens: 88_000, Window: 200_000},
			turns: []map[string]any{turn("user", "Cluster trail photos on the map"),
				turn("assistant", "Done. The map now clusters photos by trailhead, and tapping a cluster opens its gallery.")},
		},
	}
}

// runDemo keeps made-up agents in the demo folders: answering a request
// sets its agent working, and a minute later it asks again.
func runDemo() {
	agents := serveDemoAgents()
	os.MkdirAll(agentsDir(), 0o700)
	os.MkdirAll(requestsDir(), 0o700)
	os.MkdirAll(usageDir(), 0o700)
	tdir := filepath.Join(stateDir(), "transcripts")
	os.MkdirAll(tdir, 0o700)
	old, _ := filepath.Glob(filepath.Join(requestsDir(), "*"))
	for _, f := range old {
		os.Remove(f)
	}
	now := time.Now()
	plan, _ := json.Marshal(planUsage{
		FiveHour: &planWindow{Used: 42, ResetAt: now.Add(2*time.Hour + 10*time.Minute)},
		Weekly:   &planWindow{Used: 18, ResetAt: now.Add(4 * 24 * time.Hour)},
		Updated:  now,
	})
	writeAtomic(planPath(), plan)

	save := func(d *demoAgent) {
		d.agent.Updated = time.Now()
		data, _ := json.MarshalIndent(d.agent, "", "  ")
		writeAtomic(agentPath(d.agent.SessionID), data)
	}
	ask := func(d *demoAgent) {
		req := d.request()
		req.ID, req.HookPID, req.Deadline = newID(), os.Getpid(), time.Now().Add(approvalWait)
		d.reqID = req.ID
		data, _ := json.Marshal(req)
		writeAtomic(filepath.Join(requestsDir(), req.ID+".json"), data)
	}
	for _, d := range agents {
		tp := filepath.Join(tdir, d.agent.SessionID+".jsonl")
		var lines []string
		for _, t := range d.turns {
			b, _ := json.Marshal(t)
			lines = append(lines, string(b))
		}
		os.WriteFile(tp, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
		d.agent.Transcript = tp
		c, _ := json.Marshal(d.ctx)
		writeAtomic(filepath.Join(usageDir(), safeName(d.agent.SessionID)+".json"), c)
		save(d)
		if d.request != nil {
			ask(d)
		}
	}
	for range time.Tick(500 * time.Millisecond) {
		for _, d := range agents {
			if d.reqID != "" {
				res := filepath.Join(requestsDir(), d.reqID+".result.json")
				data, err := os.ReadFile(res)
				if err != nil {
					continue
				}
				var r approvalResult
				json.Unmarshal(data, &r)
				os.Remove(res)
				os.Remove(filepath.Join(requestsDir(), d.reqID+".json"))
				d.reqID, d.answered = "", time.Now()
				if r.Decision == "deny" {
					d.agent.State, d.agent.Activity = stIdle, ""
					d.agent.Said = "Okay, I won't. What would you like instead?"
				} else {
					d.agent.State, d.agent.Activity = stWorking, "Working on it"
					d.agent.Tools++
				}
				save(d)
				continue
			}
			if d.request != nil && !d.answered.IsZero() && time.Since(d.answered) > 45*time.Second {
				d.answered = time.Time{}
				d.agent.State = stWaiting
				d.agent.Activity = "Needs you again"
				save(d)
				ask(d)
			}
		}
	}
}
