# agentpad

**Claude Code, at home in [Rex](https://superlogical.com).**

agentpad doesn't reinvent the wheel. It just makes the wheel more stylish.

Claude Code's terminal UI stays exactly as it is. agentpad adds what Rex can
give it: permission prompts that come to you as drawn approval sheets,
floating over whatever you're looking at, and a board that shows what every
agent is doing right now. Everything is drawn with the kitty graphics
protocol, at your display's pixel density, in your Rex theme's colors.

<p align="center">
  <img src="docs/sheet-dark.png" width="460" alt="An approval sheet in a dark theme: Claude wants to edit plan.go, with a diff of three added lines, and Allow, Always allow and Deny buttons">
  <img src="docs/board-dark.png" width="300" alt="The agent board: one agent needs you, one is editing render.go with a subagent, one is done, one has ended">
</p>

<sub>The pictures show made-up sessions (`agentpad render`).</sub>

## Approvals that come to you

When Claude asks to run a command or change a file, a sheet floats over the
Rex window you're looking at, even if the agent is in another tab, or you're
on the iPad. It shows what Claude wants to do the way you'd want to read it:
the command in a code box, an edit as a diff, a new file's contents.

- **y** allows it, **a** always allows it (and says what that remembers,
  such as "commands starting with go test in this project"), **n** denies it.
  Clicking the buttons works too.
- **Esc** leaves it to the terminal. Claude Code's own prompt is up the whole
  time, and answering it there closes the sheet.
- The sheet ignores keys for its first moment, so typing in another pane as
  it appears can't answer it.
- If you're already looking at the agent's pane, its own prompt is right
  there, so no sheet floats.
- Anything that goes wrong (no Rex, a closed pane, a timeout) leaves the
  decision to the terminal. agentpad never decides on its own.

## Questions that come to you

When Claude asks you a multiple-choice question, it floats the same way, as
option cards. Press a number or click to choose. When a question takes
several answers, pick them and press Done. With more than one question, the
sheet goes through them in turn (← goes back). Your answers go straight to
Claude. **Esc** has the terminal ask instead, where you can also type an
answer of your own.

<p align="center">
  <img src="docs/question-dark.png" width="460" alt="A question sheet: where Saturday's route should start, with three option cards numbered 1 to 3">
</p>

## A presence for every agent

`agentpad show` docks the board beside your pane, and `agentpad float` floats
it. Every Claude Code session is a card:

- **Needs you** (amber) when it's waiting on a permission or a question,
  always at the top.
- **Working**, with what it's doing right now ("Editing render.go", "Run the
  route tests"), how many tools it has used, and its subagents.
- **Done**, with the start of its last reply.
- **Its plan,** when it keeps one: a progress ring with "4/7", and the step
  it's on. agentpad reads the agent's [rexpad](https://github.com/almonk/rexpad)
  list, or Claude's own task list in sessions that have one (newer models
  only get it with `CLAUDE_CODE_ENABLE_TODO_TOOLS=1`).
- Otherwise, your last message to it.

`j` `k` to move, **Enter** (or a click) jumps to the agent's pane, `x` forgets
a card, `q` closes the board.

## Install

You need [Rex](https://superlogical.com), Go 1.27 or later, and
[Claude Code](https://claude.com/claude-code).

```sh
git clone https://github.com/travelingflwr/agentpad
cd agentpad
make install               # builds ~/.local/bin/agentpad
agentpad install           # adds its hooks to ~/.claude/settings.json
```

`agentpad install` backs up your settings first, keeps everything else in
the file as it was, and tags its hooks so `agentpad uninstall` can take
exactly those out again. Claude Code sessions started afterwards use them.
To try it on one session first, install into another file and pass it to
Claude: `agentpad install --settings try.json`, then `claude --settings try.json`.

## How it works

- **Claude Code's hooks** run `agentpad hook` on each event (a prompt, a tool
  call, a permission request, the end of a turn). It updates one small JSON
  file per session in `~/.local/state/agentpad/agents` and exits in a few
  milliseconds, so Claude doesn't slow down.
- **A permission request** also asks Rex which session your Rex app is
  showing, floats the sheet there with `session.new_layer`, resizes it to fit
  with `session.resize_layer`, and waits for it with `rex wait`. Your answer
  goes back to Claude Code as the hook's decision.
- **A question** uses a `PreToolUse` hook on `AskUserQuestion`, which answers
  by returning the questions with your answers filled in. Claude Code shows
  its own question only after that hook returns, so the sheet is the
  question until you answer it or press Esc.
- **Answering a permission in the terminal** doesn't stop the hook, so the sheet watches
  the session's transcript for the tool call's result and closes itself.
- **The board** watches the state folder and redraws when a session changes.
  The selection is a separate small image under the list, so moving through
  it sends a few bytes, not a frame. Enter calls `session.focus_block`.

## Settings

| Variable | |
| --- | --- |
| `AGENTPAD_APPROVALS=off` | keep permission prompts in the terminal; the board still works |
| `AGENTPAD_QUESTIONS=off` | keep Claude's questions in the terminal |
| `AGENTPAD_ALWAYS_FLOAT=1` | float the sheet even over the agent's own pane |
| `AGENTPAD_THEME=light` or `dark` | fixed colors instead of following Rex |

## Credits

The kitty graphics plumbing, key decoder and theme queries come from
[rexpad](https://github.com/almonk/rexpad) by Alasdair Monk, by way of
[heypad](https://github.com/travelingflwr/heypad).

## License

[MIT](LICENSE)
