package main

// Telling when a permission prompt was answered in the terminal. Claude Code
// shows its own prompt while the approval sheet is up, and doesn't stop the
// hook when you answer there, so the sheet watches the session's transcript
// for the tool call's result and closes itself.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"reflect"
)

type transcriptWatch struct {
	path   string
	offset int64
	tool   string
	input  any
	ids    map[string]bool // tool_use IDs that are this call, with no result yet
	primed bool
}

func newTranscriptWatch(path, tool string, input json.RawMessage) *transcriptWatch {
	t := &transcriptWatch{path: path, tool: tool, ids: map[string]bool{}}
	json.Unmarshal(input, &t.input)
	// The call is usually already in the transcript; look back a little.
	if fi, err := os.Stat(path); err == nil {
		t.offset = max(0, fi.Size()-4<<20)
	}
	return t
}

// answered reads what's been added to the transcript and reports whether
// the call has a result: it ran, was denied, or was interrupted.
func (t *transcriptWatch) answered() bool {
	if t.path == "" {
		return false
	}
	f, err := os.Open(t.path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return false
	}
	r := bufio.NewReaderSize(f, 1<<16)
	done := false
	// The first read is history: an earlier identical call that was already
	// answered isn't this one, so only what's still open counts.
	defer func() { t.primed = true }()
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next time
		}
		t.offset += int64(len(line))
		if t.scan(line) {
			done = true
		}
	}
	return done && t.primed
}

type transcriptLine struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentPart struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	Input     any    `json:"input"`
	ToolUseID string `json:"tool_use_id"`
}

func (t *transcriptWatch) scan(line []byte) bool {
	if !bytes.Contains(line, []byte(`"tool_`)) {
		return false
	}
	var l transcriptLine
	if json.Unmarshal(line, &l) != nil || len(l.Message.Content) == 0 || l.Message.Content[0] != '[' {
		return false
	}
	var parts []contentPart
	if json.Unmarshal(l.Message.Content, &parts) != nil {
		return false
	}
	done := false
	for _, p := range parts {
		switch p.Type {
		case "tool_use":
			if p.Name == t.tool && reflect.DeepEqual(p.Input, t.input) {
				t.ids[p.ID] = true
			}
		case "tool_result":
			if t.ids[p.ToolUseID] {
				delete(t.ids, p.ToolUseID)
				done = len(t.ids) == 0
			}
		}
	}
	return done
}
