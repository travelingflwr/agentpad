package main

// Rex, reached through the rex CLI and its control API.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const shellFlavor = "com.superlogical.terminal.shell"

func inRex() bool { return os.Getenv("REX_SESSION") != "" }

// rexAPI calls a control API method. server is a Rex server address (from
// REX_SERVER), or "" for the one this process belongs to.
func rexAPI(server, method string, args any, out any) error {
	data, err := json.Marshal(args)
	if err != nil {
		return err
	}
	argv := []string{"api", "call"}
	if server != "" {
		argv = append(argv, "-S", server)
	}
	cmd := exec.Command("rex", append(argv, method, string(data))...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	res, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("rex %s: %s", method, strings.SplitN(msg, "\n", 2)[0])
	}
	if out != nil {
		return json.Unmarshal(res, out)
	}
	return nil
}

// paneCommand is argv wrapped so it runs with this process's environment
// rather than the Rex server's.
func paneCommand(argv ...string) []string {
	cmd := []string{"/usr/bin/env"}
	for _, k := range []string{"PATH", "HOME", "LANG", "XDG_STATE_HOME", "XDG_CONFIG_HOME", "AGENTPAD_SCALE", "AGENTPAD_THEME"} {
		if v, ok := os.LookupEnv(k); ok {
			cmd = append(cmd, k+"="+v)
		}
	}
	return append(cmd, argv...)
}

type rect [4]float64 // x, y, w, h as fractions of the window

// floatPane opens argv in a new floating layer of session's active window
// and returns the layer and block IDs. The layer goes away when argv exits.
func floatPane(server, session, label string, argv []string, bounds rect, focus bool) (layer, block string, err error) {
	var res struct {
		LayerID  string   `json:"layer_id"`
		BlockIDs []string `json:"block_ids"`
	}
	err = rexAPI(server, "session.new_layer", map[string]any{
		"session_id": session,
		"bounds":     bounds,
		"focus":      focus,
		"layout": map[string]any{"block": map[string]any{
			"flavor":  shellFlavor,
			"label":   label,
			"options": map[string]any{"command": argv, "login": false, "cwd": stateDir()},
		}},
	}, &res)
	if err != nil {
		return "", "", err
	}
	if len(res.BlockIDs) == 0 {
		return res.LayerID, "", errors.New("rex made a floating layer with no pane")
	}
	return res.LayerID, res.BlockIDs[0], nil
}

// waitBlock returns when the program in block exits.
func waitBlock(server, session, block string) {
	argv := []string{"wait"}
	if server != "" {
		argv = append(argv, "-S", server)
	}
	exec.Command("rex", append(argv, "-s", session, block)...).Run()
}

func closeLayer(server, session, layer string) {
	rexAPI(server, "session.close_layer", map[string]any{"session_id": session, "layer_id": layer}, nil)
}

func focusBlock(server, session, block string) error {
	return rexAPI(server, "session.focus_block", map[string]any{"session_id": session, "block_id": block}, nil)
}

type sessionView struct {
	ActiveWindow string `json:"active_window_id"`
	Windows      []struct {
		WindowID string `json:"window_id"`
		Label    string `json:"label"`
		Focused  string `json:"focused_block_id"`
		Layers   []struct {
			LayerID string `json:"layer_id"`
			Kind    string `json:"kind"`
			State   string `json:"state"`
			Blocks  []struct {
				BlockID string `json:"block_id"`
				Label   string `json:"label"`
			} `json:"blocks"`
		} `json:"layers"`
	} `json:"windows"`
}

func viewSession(server, session string) (sessionView, error) {
	var v sessionView
	err := rexAPI(server, "session.view", map[string]any{"session_id": session}, &v)
	return v, err
}

// focusedBlock is the block that has focus in the session's active window.
func (v sessionView) focusedBlock() string {
	for _, w := range v.Windows {
		if w.WindowID == v.ActiveWindow {
			return w.Focused
		}
	}
	return ""
}

// windowOf is the window holding block, and its label.
func (v sessionView) windowOf(block string) (id, label string) {
	for _, w := range v.Windows {
		for _, l := range w.Layers {
			for _, b := range l.Blocks {
				if b.BlockID == block {
					return w.WindowID, w.Label
				}
			}
		}
	}
	return "", ""
}

// watchedSessions are the sessions a Rex app (not a CLI) has open: where
// you are looking.
func watchedSessions(server string) []string {
	var res struct {
		Clients []struct {
			Info struct {
				Kind string `json:"kind"`
			} `json:"info"`
			State    string   `json:"connection_state"`
			Sessions []string `json:"session_ids"`
		} `json:"clients"`
	}
	if rexAPI(server, "client.list", map[string]any{}, &res) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	// The most recently connected app first: that's usually the one in hand.
	for i := len(res.Clients) - 1; i >= 0; i-- {
		c := res.Clients[i]
		if c.Info.Kind != "app" || c.State != "connected" {
			continue
		}
		for _, s := range c.Sessions {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
