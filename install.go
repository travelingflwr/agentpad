package main

// `agentpad install` adds agentpad's hooks to Claude Code's settings and
// `agentpad uninstall` takes them out, leaving everything else in the file
// as it was (keys stay in their order). Each hook is tagged "# agentpad" so
// it can be found again; installing twice replaces rather than duplicates.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const hookTag = "# agentpad"

// hookEvents are the events agentpad listens to, with each hook's timeout
// in seconds. The permission hook waits for you, so it gets longer than the
// approval sheet stays up.
var hookEvents = []struct {
	name    string
	timeout int
}{
	{"SessionStart", 5}, {"UserPromptSubmit", 5}, {"PreToolUse", 5},
	{"PermissionRequest", int(approvalWait.Seconds()) + 120},
	{"PostToolUse", 5}, {"PostToolUseFailure", 5}, {"PermissionDenied", 5},
	{"Notification", 5}, {"Stop", 5}, {"StopFailure", 5}, {"SessionEnd", 5},
}

func runInstall(args []string, remove bool) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	home, _ := os.UserHomeDir()
	path := fs.String("settings", filepath.Join(home, ".claude", "settings.json"), "the Claude Code settings file to change")
	if err := fs.Parse(args); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		self = p
	}
	data, err := os.ReadFile(*path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	out, changed, err := editHooks(data, shellQuote(self)+" hook "+hookTag, remove)
	if err != nil {
		return fmt.Errorf("%s: %w", *path, err)
	}
	if !changed {
		fmt.Println("nothing to change in", *path)
		return nil
	}
	if len(data) > 0 {
		backup := fmt.Sprintf("%s.bak-agentpad-%s", *path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(backup, data, 0o600); err != nil {
			return err
		}
		fmt.Println("backed up", *path, "to", backup)
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o755); err != nil {
		return err
	}
	if err := writeAtomic(*path, out); err != nil {
		return err
	}
	if remove {
		fmt.Println("removed agentpad's hooks from", *path)
	} else {
		fmt.Printf("added agentpad's hooks to %s for %d events.\nClaude Code sessions started from now on use them.\n", *path, len(hookEvents))
	}
	return nil
}

// editHooks returns settings with agentpad's hooks added (or removed), and
// whether anything changed.
func editHooks(settings []byte, command string, remove bool) ([]byte, bool, error) {
	if len(bytes.TrimSpace(settings)) == 0 {
		settings = []byte("{}")
	}
	top, err := parseObject(settings)
	if err != nil {
		return nil, false, err
	}
	hooksRaw, _ := top.get("hooks")
	if hooksRaw == nil {
		hooksRaw = json.RawMessage("{}")
	}
	hooks, err := parseObject(hooksRaw)
	if err != nil {
		return nil, false, fmt.Errorf("hooks: %w", err)
	}
	for _, ev := range hookEvents {
		var groups []json.RawMessage
		if raw, ok := hooks.get(ev.name); ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return nil, false, fmt.Errorf("hooks.%s: %w", ev.name, err)
			}
		}
		var kept []json.RawMessage
		for _, g := range groups {
			if !strings.Contains(string(g), hookTag) {
				kept = append(kept, g)
			}
		}
		if !remove {
			kept = append(kept, marshalPlain(map[string]any{"hooks": []any{map[string]any{
				"type": "command", "command": command, "timeout": ev.timeout,
			}}}))
		}
		if len(kept) == 0 {
			hooks.del(ev.name)
			continue
		}
		hooks.set(ev.name, json.RawMessage("["+string(bytes.Join(asBytes(kept), []byte(",")))+"]"))
	}
	if len(hooks) == 0 {
		top.del("hooks")
	} else {
		top.set("hooks", hooks.encode())
	}
	var out bytes.Buffer
	if err := json.Indent(&out, top.encode(), "", "  "); err != nil {
		return nil, false, err
	}
	out.WriteByte('\n')
	var before, after any
	json.Unmarshal(settings, &before)
	json.Unmarshal(out.Bytes(), &after)
	same, _ := json.Marshal(before)
	now, _ := json.Marshal(after)
	return out.Bytes(), !bytes.Equal(same, now), nil
}

// object is a JSON object that keeps its keys in order.
type object []struct {
	k string
	v json.RawMessage
}

func parseObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		o.set(k, v)
	}
	return o, nil
}

func (o object) get(k string) (json.RawMessage, bool) {
	for _, e := range o {
		if e.k == k {
			return e.v, true
		}
	}
	return nil, false
}

func (o *object) set(k string, v json.RawMessage) {
	for i := range *o {
		if (*o)[i].k == k {
			(*o)[i].v = v
			return
		}
	}
	*o = append(*o, struct {
		k string
		v json.RawMessage
	}{k, v})
}

func (o *object) del(k string) {
	for i := range *o {
		if (*o)[i].k == k {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

func (o object) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(e.k)
		b.Write(k)
		b.WriteByte(':')
		b.Write(e.v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// marshalPlain is json.Marshal without escaping <, > and &, which commands
// are full of.
func marshalPlain(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return bytes.TrimSpace(b.Bytes())
}

func asBytes(raws []json.RawMessage) [][]byte {
	out := make([][]byte, len(raws))
	for i, r := range raws {
		out[i] = r
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
