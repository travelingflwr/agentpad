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
	transcript := fs.String("transcript", "", "the chat: the Claude Code transcript to draw")
	input := fs.String("input", "", "the chat: what's typed in the composer")
	ask := fs.Bool("ask", false, "the chat: with a permission request to answer")
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
	case "question", "multi":
		req := demoQuestion(what == "multi")
		qs := &questionSheet{req: req}
		var in struct {
			Questions []question `json:"questions"`
		}
		json.Unmarshal(req.Input, &in)
		qs.questions = in.Questions
		qs.picked = make([]map[int]bool, len(in.Questions))
		for i := range qs.picked {
			qs.picked[i] = map[int]bool{}
		}
		if what == "multi" {
			qs.picked[0] = map[int]bool{0: true, 2: true}
			qs.cursor = 2
		}
		qs.draw(r, img, s.widthPt(), s.heightPt(), true, "")
		fmt.Printf("wants %.0f pt of %.0f\n", qs.height(r, s.widthPt()), s.heightPt())
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
	case "activity":
		v := demoActivity(r.now)
		if *live {
			v = &activityView{}
			v.reload()
		}
		v.draw(r, img, s.widthPt(), s.heightPt())
	case "chat":
		c := &chatLog{path: *transcript, byID: map[string]int{}}
		c.read()
		ch := &chatView{s: s, log: c, project: "playground", cmds: loadCommands(".")}
		ch.in.set(*input)
		if len(ch.in.text) > 6 {
			ch.in.move(len(ch.in.text) - 6) // show the cursor mid-text
		}
		if *ask {
			req := demoEdit()
			ch.ask = &inlineAsk{req: req, sheet: newSheet(req), opened: time.Now().Add(-time.Second)}
		}
		ch.still(r, img)
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
			Tasks:  []agentTask{{"1", "Find the quote styles", "done"}, {"2", "Draw quotes with a softer bar", "active"}, {"3", "Check it in light and dark", "pending"}},
			Prompt: "Give the reader a quieter quote style", Started: now.Add(-4 * time.Minute)},
		{SessionID: "3", Cwd: "/p/fieldnotes", State: stIdle, Said: "Issue 112 is drafted and the links all check out.",
			Prompt: "Draft issue 112 from my notes", Updated: now.Add(-12 * time.Minute)},
		{SessionID: "4", Cwd: "/p/garden-club", State: stEnded, Prompt: "Dedupe the seed swap list", Updated: now.Add(-50 * time.Minute)},
	}
}

func demoQuestion(multi bool) approvalRequest {
	q := map[string]any{
		"question": "Where should Saturday's route start while the north loop is closed?",
		"header":   "Trailhead",
		"options": []map[string]string{
			{"label": "Lake lot", "description": "Closest to the loop, with parking for about 40 cars. Adds a mile each way."},
			{"label": "Ranger station", "description": "Has the trail maps and restrooms, but the lot fills by 8."},
			{"label": "Ask the group", "description": "Post both options in the group thread and go with the vote."},
		},
		"multiSelect": false,
	}
	if multi {
		q = map[string]any{
			"question": "Which checks should run before the route goes out?",
			"header":   "Checks",
			"options": []map[string]string{
				{"label": "Route tests", "description": "go test ./internal/route/..."},
				{"label": "Map tiles", "description": "Make sure the lake lot is on the offline map"},
				{"label": "Weather", "description": "Saturday's forecast for the trailhead"},
			},
			"multiSelect": true,
		}
	}
	in, _ := json.Marshal(map[string]any{"questions": []any{q, q}})
	return approvalRequest{Cwd: "/Users/you/Projects/trailhead", Tool: "AskUserQuestion", Input: in}
}

func demoActivity(now time.Time) *activityView {
	agents := []Agent{
		{SessionID: "1", Cwd: "/p/trailhead", Host: "build-box", State: stWaiting, Activity: "Run the route tests",
			Prompt: "Start Saturday's route from the lake lot", Updated: now.Add(-2 * time.Minute)},
		{SessionID: "2", Cwd: "/p/fieldnotes", State: stWaiting, Activity: "AskUserQuestion",
			Prompt: "Draft issue 112 from my notes", Updated: now.Add(-9 * time.Minute)},
		{SessionID: "3", Cwd: "/p/heypad", State: stWorking, Activity: "Editing render.go",
			Prompt: "Give the reader a quieter quote style", Started: now.Add(-14 * time.Minute)},
		{SessionID: "4", Cwd: "/p/garden-club", Host: "build-box", State: stWorking, Activity: "Reading seeds.csv",
			Prompt: "Dedupe the seed swap list", Started: now.Add(-6 * time.Minute)},
		{SessionID: "5", Cwd: "/p/trailhead", Host: "lab-server", State: stWorking, Activity: "Running go test",
			Prompt: "Profile the slow map startup", Started: now.Add(-1 * time.Minute)},
		{SessionID: "6", Cwd: "/p/fieldnotes", State: stIdle, Prompt: "Write the release notes", Updated: now.Add(-4 * time.Minute)},
	}
	bash := demoBash()
	bash.SessionID = "1"
	in, _ := json.Marshal(map[string]any{"questions": []any{map[string]any{
		"question": "Which branch should the pull request go to, main or release-2.4?", "header": "Branch",
		"options": []map[string]string{{"label": "main"}, {"label": "release-2.4"}}}}})
	q := approvalRequest{SessionID: "2", Cwd: "/p/fieldnotes", Tool: "AskUserQuestion", Input: in}
	return &activityView{
		agents: agents,
		pending: map[string]*pendingRequest{
			"1": {req: bash, opened: now.Add(-2 * time.Minute)},
			"2": {req: q, opened: now.Add(-9 * time.Minute)},
		},
		plan: planUsage{
			FiveHour: &planWindow{Used: 62, ResetAt: now.Add(2*time.Hour + 10*time.Minute)},
			Weekly:   &planWindow{Used: 41, ResetAt: now.Add(4 * 24 * time.Hour)},
		},
		hasPlan: true,
		host:    "this-mac",
		demoCtx: map[string]contextUse{
			"1": {108_000, 200_000}, "2": {64_000, 200_000}, "3": {182_000, 200_000},
			"4": {74_000, 200_000}, "5": {31_000, 1_000_000},
		},
	}
}
