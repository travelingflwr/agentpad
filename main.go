// agentpad gives Claude Code a presence in Rex: permission prompts float
// over whatever you're looking at as drawn approval sheets, and a board pane
// shows what every agent is doing. Claude Code's own TUI stays as it is.
package main

import (
	"fmt"
	"os"
	"strings"
)

const usage = `agentpad: Claude Code, at home in Rex

  agentpad chat [--continue] [DIR] [-- FLAGS]Claude Code as a Rex surface: a drawn chat, with
                                          the real Claude Code running in a tab beside it
  agentpad show [--below] [--ratio PCT]   the agent board in a pane beside yours
  agentpad float                          the agent board in a floating window (again to close)
  agentpad board                          run the board in this pane

  agentpad install [--settings FILE]      add agentpad's hooks to Claude Code's settings
  agentpad uninstall [--settings FILE]    take them out again
  agentpad render (sheet|board) -o FILE   draw a demo picture, for checking the look

  agentpad hook                           (run by Claude Code's hooks)
  agentpad approve REQUEST                (the approval sheet's pane)

Environment: AGENTPAD_APPROVALS=off keeps permission prompts in the terminal;
AGENTPAD_ALWAYS_FLOAT=1 floats them even over the agent's own pane;
AGENTPAD_THEME=light|dark pins the colors instead of following Rex.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		return
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "hook":
		err = runHook(args)
	case "approve":
		err = runApprove(args)
	case "chat":
		err = runChat(args)
	case "board", "view":
		err = runBoard(args)
	case "show":
		err = runShow(args)
	case "float":
		err = runFloat(args)
	case "install":
		err = runInstall(args, false)
	case "uninstall":
		err = runInstall(args, true)
	case "render":
		err = runRender(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "agentpad: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentpad:", strings.TrimSpace(err.Error()))
		os.Exit(1)
	}
}
