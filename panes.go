package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const boardLabel = "Agents"

var boardBounds = rect{0.66, 0.05, 0.32, 0.6}

func needRex() error {
	if !inRex() {
		return errors.New("not running inside Rex; run `agentpad board` in a terminal with kitty graphics")
	}
	return nil
}

// runShow opens the board in a split beside this pane.
func runShow(args []string) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	below := fs.Bool("below", false, "split downward instead of to the right")
	ratio := fs.Int("ratio", 30, "the board's share of the pane, in percent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := needRex(); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	split := "right"
	if *below {
		split = "down"
	}
	argv := []string{"split", "--focus=false", "--split", split, "--ratio", fmt.Sprint(*ratio), "--shell=none", "--label", boardLabel}
	if b := os.Getenv("REX_BLOCK"); b != "" {
		argv = append(argv, "-b", b)
	}
	argv = append(argv, "--", self, "board")
	cmd := exec.Command("rex", argv...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("rex split: %w", err)
	}
	fmt.Println(strings.TrimSpace(string(out)))
	return nil
}

// runFloat opens the board as a floating window, or closes it.
func runFloat(args []string) error {
	if err := needRex(); err != nil {
		return err
	}
	session := os.Getenv("REX_SESSION")
	if v, err := viewSession("", session); err == nil {
		for _, w := range v.Windows {
			if w.WindowID != v.ActiveWindow {
				continue
			}
			for _, l := range w.Layers {
				if l.Kind == "floating" && len(l.Blocks) == 1 && l.Blocks[0].Label == boardLabel {
					closeLayer("", session, l.LayerID)
					return nil
				}
			}
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	_, _, err = floatPane("", session, boardLabel, paneCommand(self, "board"), boardBounds, true)
	return err
}
