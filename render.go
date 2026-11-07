package main

// `agentpad render` draws a made-up approval sheet or board to a PNG, so the
// look can be checked (and shown) without a real session.

import (
	"encoding/json"
	"flag"
	"fmt"
	"image/png"
	"os"
	"time"
)

func runRender(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: agentpad render (sheet|bash|board) -o FILE")
	}
	what := args[0]
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	out := fs.String("o", "agentpad.png", "where to write the PNG")
	theme := fs.String("theme", "dark", "light or dark")
	cols := fs.Int("cols", 64, "pane width, in cells")
	rows := fs.Int("rows", 26, "pane height, in cells")
	live := fs.Bool("live", false, "the board: draw the real sessions instead of made-up ones")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	fonts, err := loadFonts()
	if err != nil {
		return err
	}
	mono, err := loadMono()
	if err != nil {
		return err
	}
	s := &screen{ws: winSize{rows: *rows, cols: *cols, xpix: *cols * 16, ypix: *rows * 34}, scale: 2,
		theme: *theme, fonts: fonts, mono: mono}
	s.pal = s.colors.palette(*theme)
	r := s.renderer()
	img := s.canvas(s.heightPt())
	r.paint(img)
	switch what {
	case "sheet", "bash":
		req := demoEdit()
		if what == "bash" {
			req = demoBash()
		}
		newSheet(req).draw(r, img, s.widthPt(), s.heightPt(), true, "")
	case "board":
		lay := layoutBoard(s)
		agents := demoAgents(r.now)
		if *live {
			agents = loadAgents()
		}
		sel := 1
		y := lay.header + float64(sel)*lay.pitch
		r.fillRoundRect(img, selInset, y+3, s.widthPt()-selInset, y+lay.pitch-3, selRadius, r.pal.selBg)
		drawBoard(r, img, agents, s.widthPt(), lay, "")
	default:
		return fmt.Errorf("render what? sheet, bash or board")
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func demoEdit() approvalRequest {
	in, _ := json.Marshal(map[string]any{
		"file_path":  "/Users/you/Projects/trailhead/internal/route/plan.go",
		"old_string": "func (p *Plan) Start() Point {\n\tif len(p.Stops) == 0 {\n\t\treturn p.Origin\n\t}\n\treturn p.Stops[0]\n}",
		"new_string": "func (p *Plan) Start() Point {\n\tif p.Closed(\"north-loop\") {\n\t\treturn p.Lot(\"lake\")\n\t}\n\tif len(p.Stops) == 0 {\n\t\treturn p.Origin\n\t}\n\treturn p.Stops[0]\n}",
	})
	sugg, _ := json.Marshal([]map[string]any{{"type": "setMode", "mode": "acceptEdits", "destination": "session"}})
	return approvalRequest{Cwd: "/Users/you/Projects/trailhead", Tool: "Edit", Input: in, Suggestions: sugg}
}

func demoBash() approvalRequest {
	in, _ := json.Marshal(map[string]any{
		"command":     "go test ./internal/route/... -run TestClosedTrail -count=1",
		"description": "Run the route tests for the trail closure",
	})
	sugg, _ := json.Marshal([]map[string]any{{"type": "addRules", "behavior": "allow", "destination": "localSettings",
		"rules": []map[string]any{{"toolName": "Bash", "ruleContent": "go test:*"}}}})
	return approvalRequest{Cwd: "/Users/you/Projects/trailhead", Tool: "Bash", Input: in, Suggestions: sugg, Elsewhere: "Window 2"}
}

func demoAgents(now time.Time) []Agent {
	return []Agent{
		{SessionID: "1", Cwd: "/p/trailhead", State: stWaiting, Activity: "Run the route tests for the trail closure",
			Prompt: "Start Saturday's route from the lake lot while the north loop is closed", Updated: now.Add(-20 * time.Second), Tools: 14},
		{SessionID: "2", Cwd: "/p/heypad", State: stWorking, Activity: "Editing render.go", Tools: 23, Subagents: 1,
			Prompt: "Give the reader a quieter quote style", Started: now.Add(-4 * time.Minute)},
		{SessionID: "3", Cwd: "/p/fieldnotes", State: stIdle, Said: "Issue 112 is drafted and the links all check out.",
			Prompt: "Draft issue 112 from my notes", Updated: now.Add(-12 * time.Minute)},
		{SessionID: "4", Cwd: "/p/garden-club", State: stEnded, Prompt: "Dedupe the seed swap list", Updated: now.Add(-50 * time.Minute)},
	}
}
