package main

// The slash-command menu: Claude Code's built-in commands, plus the
// commands and skills in your settings, your plugins and the project, as
// Claude Code would offer them.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type slashCommand struct {
	name, desc string
	terminal   bool // opens its own screen in Claude Code: show the terminal
}

var builtinCommands = []slashCommand{
	{"/add-dir", "Add another folder Claude can work in", true},
	{"/agents", "Manage subagents", true},
	{"/clear", "Start a new conversation", false},
	{"/compact", "Summarize the conversation to free up context", false},
	{"/config", "Open settings", true},
	{"/context", "Show what's using the context window", false},
	{"/cost", "Show this session's cost and duration", false},
	{"/doctor", "Check Claude Code's installation", true},
	{"/effort", "Set how hard Claude thinks", true},
	{"/exit", "Quit Claude Code", false},
	{"/export", "Export the conversation", true},
	{"/help", "Show help", true},
	{"/hooks", "Manage hooks", true},
	{"/init", "Write a CLAUDE.md for this project", false},
	{"/mcp", "Manage MCP servers", true},
	{"/memory", "Edit memory files", true},
	{"/model", "Choose the model", true},
	{"/permissions", "Manage permission rules", true},
	{"/resume", "Resume an earlier conversation", true},
	{"/review", "Review a pull request", false},
	{"/rewind", "Go back to an earlier point", true},
	{"/security-review", "Review pending changes for security issues", false},
	{"/status", "Show version, model and account", true},
	{"/usage", "Show plan usage", true},
}

// loadCommands is every command for a project in dir.
func loadCommands(dir string) []slashCommand {
	home, _ := os.UserHomeDir()
	cmds := append([]slashCommand(nil), builtinCommands...)
	seen := map[string]bool{}
	for _, c := range cmds {
		seen[c.name] = true
	}
	add := func(name, desc string) {
		if name == "/" || seen[name] {
			return
		}
		seen[name] = true
		cmds = append(cmds, slashCommand{name: name, desc: desc})
	}
	// Commands and skills under a .claude-style folder, with an optional
	// plugin prefix.
	scan := func(root, prefix string) {
		if files, _ := filepath.Glob(filepath.Join(root, "commands", "*.md")); files != nil {
			for _, f := range files {
				add("/"+prefix+strings.TrimSuffix(filepath.Base(f), ".md"), frontDesc(f))
			}
		}
		if files, _ := filepath.Glob(filepath.Join(root, "skills", "*", "SKILL.md")); files != nil {
			for _, f := range files {
				add("/"+prefix+filepath.Base(filepath.Dir(f)), frontDesc(f))
			}
		}
	}
	scan(filepath.Join(dir, ".claude"), "")
	scan(filepath.Join(home, ".claude"), "")

	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json")); err == nil {
		json.Unmarshal(data, &installed)
	}
	names := make([]string, 0, len(installed.Plugins))
	for n := range installed.Plugins {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, p := range installed.Plugins[n] {
			scan(p.InstallPath, strings.SplitN(n, "@", 2)[0]+":")
		}
	}
	if synced, _ := filepath.Glob(filepath.Join(home, ".claude", "plugins", "synced", "*", "*")); synced != nil {
		for _, p := range synced {
			scan(p, strings.SplitN(filepath.Base(p), "~", 2)[0]+":")
		}
	}
	return cmds
}

// frontDesc is the description in a markdown file's front matter.
func frontDesc(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	in := false
	for n := 0; sc.Scan() && n < 40; n++ {
		l := strings.TrimSpace(sc.Text())
		if l == "---" {
			if in {
				break
			}
			in = true
			continue
		}
		if in && strings.HasPrefix(l, "description:") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "description:")), `"'`)
		}
	}
	return ""
}

// matchCommands is what fits what's typed so far: names that start with it
// first, then names with it anywhere.
func matchCommands(cmds []slashCommand, typed string) []slashCommand {
	typed = strings.ToLower(typed)
	var first, rest []slashCommand
	for _, c := range cmds {
		n := strings.ToLower(c.name)
		short := n
		if i := strings.Index(n, ":"); i >= 0 {
			short = "/" + n[i+1:]
		}
		switch {
		case strings.HasPrefix(n, typed), strings.HasPrefix(short, typed):
			first = append(first, c)
		case strings.Contains(n, strings.TrimPrefix(typed, "/")):
			rest = append(rest, c)
		}
	}
	return append(first, rest...)
}
