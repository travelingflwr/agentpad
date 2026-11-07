package main

// `agentpad hook` is what Claude Code's hooks run. Every event updates the
// session's record for the board. A permission request also floats an
// approval sheet over the Rex window you're looking at and waits for your
// answer there, while Claude Code shows its usual prompt; whichever you
// answer first wins. Anything that goes wrong leaves the decision to the
// terminal: the hook never blocks Claude Code on its own account.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// approvalBounds is where the sheet floats: the lower right of the window.
var approvalBounds = rect{0.44, 0.3, 0.54, 0.66}

// approvalWait is how long a sheet stays up. The installed hook's timeout is
// a little longer, so the sheet always closes first.
const approvalWait = 25 * time.Minute

type approvalRequest struct {
	ID          string          `json:"id"`
	SessionID   string          `json:"session_id"`
	Cwd         string          `json:"cwd"`
	Transcript  string          `json:"transcript_path"`
	Tool        string          `json:"tool_name"`
	Input       json.RawMessage `json:"tool_input"`
	Suggestions json.RawMessage `json:"permission_suggestions,omitempty"`
	Subagent    bool            `json:"subagent,omitempty"`
	HookPID     int             `json:"hook_pid"`
	Deadline    time.Time       `json:"deadline"`
	// Elsewhere is set when the agent's pane is in another session than
	// the one the sheet floats over: its window's label.
	Elsewhere string `json:"elsewhere,omitempty"`
}

type approvalResult struct {
	Decision string `json:"decision"` // allow, always, deny, or terminal
	Note     string `json:"note,omitempty"`
}

func requestsDir() string { return filepath.Join(stateDir(), "requests") }

func runHook(args []string) error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
	if err != nil {
		return nil
	}
	var in hookInput
	if json.Unmarshal(data, &in) != nil {
		return nil
	}
	if err := recordEvent(in); err != nil {
		logf("record %s: %v", in.Event, err)
	}
	if in.Event == "PermissionRequest" {
		if out := askForApproval(in); out != nil {
			os.Stdout.Write(out)
		}
	}
	return nil
}

func logf(format string, args ...any) {
	os.MkdirAll(stateDir(), 0o700)
	f, err := os.OpenFile(filepath.Join(stateDir(), "hook.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// askForApproval floats the sheet and returns the hook's output, or nil to
// leave the decision to the terminal.
func askForApproval(in hookInput) []byte {
	if !inRex() || os.Getenv("AGENTPAD_APPROVALS") == "off" || asksYou(in.ToolName) {
		return nil
	}
	own, block := os.Getenv("REX_SESSION"), os.Getenv("REX_BLOCK")
	target := own
	watched := watchedSessions("")
	if !contains(watched, own) && len(watched) > 0 {
		target = watched[0] // you're looking at another session: go there
	}
	req := approvalRequest{
		ID: newID(), SessionID: in.SessionID, Cwd: in.Cwd, Transcript: in.TranscriptPath,
		Tool: in.ToolName, Input: in.ToolInput, Suggestions: in.Suggestions,
		Subagent: in.AgentID != "", HookPID: os.Getpid(), Deadline: time.Now().Add(approvalWait),
	}
	if target == own {
		// Watching this very pane? Its own prompt is right there.
		if v, err := viewSession("", own); err == nil && v.focusedBlock() == block && contains(watched, own) &&
			os.Getenv("AGENTPAD_ALWAYS_FLOAT") == "" {
			return nil
		}
	} else if v, err := viewSession("", own); err == nil {
		_, req.Elsewhere = v.windowOf(block)
		if req.Elsewhere == "" {
			req.Elsewhere = "another session"
		}
	}

	if err := os.MkdirAll(requestsDir(), 0o700); err != nil {
		return nil
	}
	reqPath := filepath.Join(requestsDir(), req.ID+".json")
	resPath := filepath.Join(requestsDir(), req.ID+".result.json")
	data, _ := json.Marshal(req)
	if err := writeAtomic(reqPath, data); err != nil {
		return nil
	}
	defer os.Remove(reqPath)
	defer os.Remove(resPath)

	self, err := os.Executable()
	if err != nil {
		return nil
	}
	layer, sheet, err := floatPane("", target, "Approve", paneCommand(self, "approve", reqPath), approvalBounds, true)
	if err != nil {
		logf("float approval: %v", err)
		return nil
	}
	waitBlock("", target, sheet)
	closeLayer("", target, layer) // in case the sheet failed to start

	var res approvalResult
	if data, err := os.ReadFile(resPath); err != nil || json.Unmarshal(data, &res) != nil {
		return nil
	}
	return decisionJSON(res, in.Suggestions)
}

func decisionJSON(res approvalResult, suggestions json.RawMessage) []byte {
	decision := map[string]any{}
	switch res.Decision {
	case "allow":
		decision["behavior"] = "allow"
	case "always":
		decision["behavior"] = "allow"
		if len(suggestions) > 0 && string(suggestions) != "null" {
			decision["updatedPermissions"] = suggestions
		}
	case "deny":
		decision["behavior"] = "deny"
		msg := "The user denied this from the approval sheet in Rex."
		if res.Note != "" {
			msg += " They said: " + res.Note
		}
		decision["message"] = msg
	default:
		return nil
	}
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PermissionRequest", "decision": decision,
	}})
	return out
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
