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
	"syscall"
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
	// Inline is set when the agent's chat is what you're looking at: the
	// chat shows the request in the conversation, with no sheet.
	Inline bool `json:"inline,omitempty"`
	// Remote is set when no sheet floats and only the phone (through
	// agentpad serve) and the terminal can answer.
	Remote bool `json:"remote,omitempty"`
}

type approvalResult struct {
	Decision string            `json:"decision"` // allow, always, deny, answer, or terminal
	Note     string            `json:"note,omitempty"`
	Answers  map[string]string `json:"answers,omitempty"` // a question's text to the chosen labels
	Via      string            `json:"via,omitempty"`     // the paired device that answered, if one did
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
	// `hook question` is the long-running hook for Claude's questions;
	// the everyday hook has already recorded the event.
	if len(args) > 0 && args[0] == "question" {
		if in.Event == "PreToolUse" && in.ToolName == "AskUserQuestion" {
			if out := askQuestion(in); out != nil {
				os.Stdout.Write(out)
			}
		}
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
	if os.Getenv("AGENTPAD_APPROVALS") == "off" || asksYou(in.ToolName) {
		return nil
	}
	res, ok := floatSheet(in, "Approve")
	if !ok {
		return nil
	}
	return decisionJSON(res, in.Suggestions)
}

// askQuestion floats Claude's question and returns its answer as the hook's
// output, or nil to let the terminal ask. Claude Code shows its own question
// only after this hook returns.
func askQuestion(in hookInput) []byte {
	if os.Getenv("AGENTPAD_QUESTIONS") == "off" {
		return nil
	}
	res, ok := floatSheet(in, "Question")
	if !ok || res.Decision != "answer" || len(res.Answers) == 0 {
		return nil
	}
	var input map[string]any
	if json.Unmarshal(in.ToolInput, &input) != nil {
		return nil
	}
	input["answers"] = res.Answers
	out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "allow", "updatedInput": input,
	}})
	return out
}

// floatSheet floats a sheet for in over the Rex window you're looking at and
// waits for your answer. ok is false when no sheet was shown: outside Rex,
// when you're already looking at the agent's pane, or on any error.
func floatSheet(in hookInput, label string) (approvalResult, bool) {
	if !inRex() {
		return remoteOnly(in, label)
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
	chat, haveChat := chatSurface(own)
	if haveChat {
		// An agentpad chat is this agent's face: the sheet floats over it,
		// or, when you're looking at the chat, the chat asks you itself.
		target = chat.Session
		if v, err := viewSession("", chat.Session); err == nil && v.focusedBlock() == chat.Block && label == "Approve" {
			req.Inline = true
		}
	} else if target == own {
		// Watching this very pane? Its own prompt is right there.
		if v, err := viewSession("", own); err == nil && v.focusedBlock() == block && contains(watched, own) &&
			os.Getenv("AGENTPAD_ALWAYS_FLOAT") == "" {
			return remoteOnly(in, label)
		}
	} else if v, err := viewSession("", own); err == nil {
		_, req.Elsewhere = v.windowOf(block)
		if req.Elsewhere == "" {
			req.Elsewhere = "another session"
		}
	}

	if err := os.MkdirAll(requestsDir(), 0o700); err != nil {
		return approvalResult{}, false
	}
	reqPath := filepath.Join(requestsDir(), req.ID+".json")
	resPath := filepath.Join(requestsDir(), req.ID+".result.json")
	data, _ := json.Marshal(req)
	if err := writeAtomic(reqPath, data); err != nil {
		return approvalResult{}, false
	}
	defer os.Remove(reqPath)
	defer os.Remove(resPath)

	if req.Inline {
		return awaitInline(req, resPath, chat.PID)
	}

	self, err := os.Executable()
	if err != nil {
		return approvalResult{}, false
	}
	layer, sheet, err := floatPane("", target, label, paneCommand(self, "approve", reqPath), approvalBounds, true)
	if err != nil {
		logf("float %s: %v", label, err)
		if label == "Approve" && serveActive() {
			return awaitResult(req, resPath) // the phone can still answer
		}
		return approvalResult{}, false
	}
	waitBlock("", target, sheet)
	closeLayer("", target, layer) // in case the sheet failed to start

	var res approvalResult
	if data, err := os.ReadFile(resPath); err != nil || json.Unmarshal(data, &res) != nil {
		return approvalResult{}, false
	}
	return res, true
}

type surface struct {
	Session string `json:"session"`
	Block   string `json:"block"`
	PID     int    `json:"pid"`
}

// chatSurface is the agentpad chat drawing the Claude Code in session, when
// one is open.
func chatSurface(session string) (surface, bool) {
	var sf surface
	data, err := os.ReadFile(surfacePath(session))
	if err != nil {
		return sf, false
	}
	if json.Unmarshal(data, &sf) != nil || sf.Session == "" || sf.PID <= 0 || syscall.Kill(sf.PID, 0) != nil {
		return sf, false // gone without tidying up
	}
	return sf, true
}

// remoteOnly is for when no sheet floats: outside Rex, or when you're
// looking at the agent's own pane. With agentpad serve running and a phone
// paired, a permission request still goes to the phone, while Claude Code's
// prompt stays up in the terminal; whichever you answer first wins.
// Questions don't: Claude Code shows its own question only once the hook
// returns, so holding the hook would hide the question from the terminal.
func remoteOnly(in hookInput, label string) (approvalResult, bool) {
	if label != "Approve" || !serveActive() {
		return approvalResult{}, false
	}
	req := approvalRequest{
		ID: newID(), SessionID: in.SessionID, Cwd: in.Cwd, Transcript: in.TranscriptPath,
		Tool: in.ToolName, Input: in.ToolInput, Suggestions: in.Suggestions,
		Subagent: in.AgentID != "", HookPID: os.Getpid(), Deadline: time.Now().Add(approvalWait),
		Remote: true,
	}
	if err := os.MkdirAll(requestsDir(), 0o700); err != nil {
		return approvalResult{}, false
	}
	reqPath := filepath.Join(requestsDir(), req.ID+".json")
	resPath := filepath.Join(requestsDir(), req.ID+".result.json")
	data, _ := json.Marshal(req)
	if err := writeAtomic(reqPath, data); err != nil {
		return approvalResult{}, false
	}
	defer os.Remove(reqPath)
	defer os.Remove(resPath)
	return awaitResult(req, resPath)
}

// awaitResult waits for an answer to req from anywhere but a sheet: until
// the result appears, the prompt is answered in the terminal, or time's up.
func awaitResult(req approvalRequest, resPath string) (approvalResult, bool) {
	watch := newTranscriptWatch(req.Transcript, req.Tool, req.Input)
	watch.answered() // read the history
	for time.Now().Before(req.Deadline) {
		time.Sleep(150 * time.Millisecond)
		if data, err := os.ReadFile(resPath); err == nil {
			var res approvalResult
			if json.Unmarshal(data, &res) == nil {
				return res, true
			}
		}
		if watch.answered() {
			return approvalResult{}, false
		}
	}
	return approvalResult{}, false
}

// awaitInline waits for the chat to answer req: until the result appears,
// the prompt is answered in the terminal, the chat closes, or time's up.
func awaitInline(req approvalRequest, resPath string, chatPID int) (approvalResult, bool) {
	watch := newTranscriptWatch(req.Transcript, req.Tool, req.Input)
	watch.answered() // read the history
	for time.Now().Before(req.Deadline) {
		time.Sleep(150 * time.Millisecond)
		if data, err := os.ReadFile(resPath); err == nil {
			var res approvalResult
			if json.Unmarshal(data, &res) == nil {
				return res, true
			}
		}
		if watch.answered() || syscall.Kill(chatPID, 0) != nil {
			return approvalResult{}, false
		}
	}
	return approvalResult{}, false
}

func surfacePath(session string) string {
	return filepath.Join(stateDir(), "surfaces", safeName(session)+".json")
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
		if res.Via != "" {
			msg = "The user denied this remotely, from " + res.Via + "."
		}
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
