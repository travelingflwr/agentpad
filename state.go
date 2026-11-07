package main

// What each Claude Code session is doing, kept as one small JSON file per
// session. Claude Code's hooks write it (agentpad hook) and the board reads
// it. The files live in ~/.local/state/agentpad/agents.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

type agentState string

const (
	stWorking agentState = "working"
	stWaiting agentState = "waiting" // asking you for permission
	stIdle    agentState = "idle"    // finished its turn, your move
	stEnded   agentState = "ended"
)

type Agent struct {
	SessionID  string     `json:"session_id"`
	Cwd        string     `json:"cwd"`
	Transcript string     `json:"transcript_path,omitempty"`
	RexSession string     `json:"rex_session,omitempty"`
	RexBlock   string     `json:"rex_block,omitempty"`
	RexServer  string     `json:"rex_server,omitempty"`
	Host       string     `json:"host,omitempty"`
	State      agentState `json:"state"`
	Activity   string     `json:"activity,omitempty"` // what it's doing, or what it's asking
	Prompt     string     `json:"prompt,omitempty"`   // your last message
	Said       string     `json:"said,omitempty"`     // the start of its last reply
	Tools      int        `json:"tools"`              // tool calls this turn
	Subagents  int        `json:"subagents,omitempty"`
	Started    time.Time  `json:"started"`           // when this turn began
	Updated    time.Time  `json:"updated"`           // the last event
	Waiting    int        `json:"waiting,omitempty"` // open permission requests
}

// Project is the folder the agent works in, by name.
func (a Agent) Project() string {
	if a.Cwd == "" {
		return "Claude"
	}
	return filepath.Base(a.Cwd)
}

func stateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "agentpad")
}

func agentsDir() string { return filepath.Join(stateDir(), "agents") }

func agentPath(session string) string {
	return filepath.Join(agentsDir(), safeName(session)+".json")
}

// safeName keeps a session ID usable as a file name, whatever it holds.
func safeName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			b.WriteRune(c)
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// updateAgent changes one session's record under a lock, since Claude Code
// runs hooks for parallel tool calls at the same time.
func updateAgent(session string, change func(a *Agent)) (Agent, error) {
	if err := os.MkdirAll(agentsDir(), 0o700); err != nil {
		return Agent{}, err
	}
	path := agentPath(session)
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return Agent{}, err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return Agent{}, err
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)

	var a Agent
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &a)
	}
	a.SessionID = session
	change(&a)
	a.Updated = time.Now()
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return a, err
	}
	return a, writeAtomic(path, data)
}

func writeAtomic(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadAgents reads every session's record, waiting first, then working,
// then the rest, most recent first within each.
func loadAgents() []Agent {
	paths, _ := filepath.Glob(filepath.Join(agentsDir(), "*.json"))
	var out []Agent
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a Agent
		if json.Unmarshal(data, &a) == nil && a.SessionID != "" {
			out = append(out, a)
		}
	}
	rank := map[agentState]int{stWaiting: 0, stWorking: 1, stIdle: 2, stEnded: 3}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].State], rank[out[j].State]
		if ri != rj {
			return ri < rj
		}
		return out[i].Updated.After(out[j].Updated)
	})
	return out
}

// forgetAgent removes a session's record.
func forgetAgent(session string) {
	os.Remove(agentPath(session))
	os.Remove(agentPath(session) + ".lock")
}

// pruneAgents forgets sessions that ended over an hour ago, or that haven't
// been heard from in a day.
func pruneAgents(agents []Agent, now time.Time) {
	for _, a := range agents {
		if a.State == stEnded && now.Sub(a.Updated) > time.Hour || now.Sub(a.Updated) > 24*time.Hour {
			forgetAgent(a.SessionID)
		}
	}
}

// --- hook events ----------------------------------------------------------

// hookInput is what Claude Code sends a hook on stdin; each event fills in
// some of it.
type hookInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	Event          string          `json:"hook_event_name"`
	PermissionMode string          `json:"permission_mode"`
	AgentID        string          `json:"agent_id"`
	Source         string          `json:"source"`
	Prompt         string          `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
	Suggestions    json.RawMessage `json:"permission_suggestions"`
	Notification   string          `json:"notification_type"`
	Message        string          `json:"message"`
	LastMessage    string          `json:"last_assistant_message"`
	Reason         string          `json:"reason"`
}

// recordEvent updates the session's record for one hook event.
func recordEvent(in hookInput) error {
	if in.SessionID == "" {
		return errors.New("no session_id")
	}
	if in.Event == "SessionEnd" && in.Reason == "clear" {
		// /clear ends one conversation and starts another in the same pane.
		forgetAgent(in.SessionID)
		return nil
	}
	_, err := updateAgent(in.SessionID, func(a *Agent) {
		if in.Cwd != "" && in.AgentID == "" {
			a.Cwd = in.Cwd
		}
		if in.TranscriptPath != "" && in.AgentID == "" {
			a.Transcript = in.TranscriptPath
		}
		if v := os.Getenv("REX_SESSION"); v != "" {
			a.RexSession, a.RexBlock, a.RexServer = v, os.Getenv("REX_BLOCK"), os.Getenv("REX_SERVER")
		}
		if a.Host == "" {
			a.Host, _ = os.Hostname()
		}
		switch in.Event {
		case "SessionStart":
			if a.State == "" || a.State == stEnded {
				a.State, a.Activity = stIdle, ""
				a.Started = time.Now()
			}
		case "UserPromptSubmit":
			a.State, a.Activity, a.Said = stWorking, "Thinking", ""
			a.Prompt = oneLine(in.Prompt, 240)
			a.Tools, a.Waiting, a.Subagents = 0, 0, 0
			a.Started = time.Now()
		case "PreToolUse":
			a.Tools++
			if a.Waiting == 0 {
				a.State = stWorking
			}
			if in.ToolName == "Agent" || in.ToolName == "Task" {
				a.Subagents++
			}
			if in.AgentID == "" || a.Activity == "" || a.Activity == "Thinking" {
				a.Activity = describeTool(in.ToolName, in.ToolInput, a.Cwd)
			}
			if asksYou(in.ToolName) {
				a.State = stWaiting
				a.Waiting++
			}
		case "PermissionRequest":
			if asksYou(in.ToolName) {
				break // PreToolUse counted it
			}
			a.State = stWaiting
			a.Waiting++
			a.Activity = describeTool(in.ToolName, in.ToolInput, a.Cwd)
		case "PostToolUse", "PostToolUseFailure", "PermissionDenied":
			if a.Waiting > 0 {
				a.Waiting--
			}
			if a.Waiting == 0 && a.State != stIdle {
				a.State = stWorking
			}
		case "Notification":
			switch in.Notification {
			case "idle_prompt":
				a.State = stIdle
			}
		case "Stop", "StopFailure":
			a.State, a.Waiting = stIdle, 0
			if in.LastMessage != "" {
				a.Said = oneLine(stripMarkdown(in.LastMessage), 240)
			}
			if in.Event == "StopFailure" {
				a.Activity = "Stopped with an error"
			} else {
				a.Activity = ""
			}
		case "SessionEnd":
			a.State, a.Waiting, a.Activity = stEnded, 0, ""
		}
	})
	return err
}

// asksYou is whether a tool is itself a question for you, answered in the
// terminal rather than approved.
func asksYou(tool string) bool { return tool == "AskUserQuestion" || tool == "ExitPlanMode" }

// describeTool says in a few words what a tool call does.
func describeTool(name string, raw json.RawMessage, cwd string) string {
	var in map[string]any
	json.Unmarshal(raw, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	rel := func(p string) string {
		if cwd != "" {
			if r, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(r, "..") {
				return r
			}
		}
		return filepath.Base(p)
	}
	switch name {
	case "Bash":
		if d := str("description"); d != "" {
			return oneLine(d, 80)
		}
		return "Running " + oneLine(str("command"), 60)
	case "Edit", "MultiEdit":
		return "Editing " + rel(str("file_path"))
	case "Write":
		return "Writing " + rel(str("file_path"))
	case "Read":
		return "Reading " + rel(str("file_path"))
	case "NotebookEdit":
		return "Editing " + rel(str("notebook_path"))
	case "Grep", "Glob":
		return "Searching for " + oneLine(str("pattern"), 50)
	case "WebFetch":
		if u, err := url.Parse(str("url")); err == nil && u.Host != "" {
			return "Reading " + u.Host
		}
		return "Reading the web"
	case "WebSearch":
		return "Searching the web for " + oneLine(str("query"), 50)
	case "Agent", "Task":
		if d := str("description"); d != "" {
			return "Subagent: " + oneLine(d, 60)
		}
		return "Running a subagent"
	case "Skill":
		return "Using the " + str("skill") + " skill"
	case "ToolSearch":
		return "Loading tools"
	case "AskUserQuestion":
		return "Asking you a question"
	case "ExitPlanMode":
		return "Presenting a plan"
	}
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.Split(name, "__")
		return strings.ReplaceAll(parts[len(parts)-1], "_", " ")
	}
	return name
}

var markdownMarks = strings.NewReplacer("`", "", "**", "", "__", "", "## ", "", "# ", "")

// stripMarkdown drops the marks that would show as clutter in one line.
func stripMarkdown(s string) string { return markdownMarks.Replace(s) }

// oneLine folds s onto one line and cuts it to about n characters.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)[:n]
	return strings.TrimRight(string(r), " ,.;:") + "…"
}
