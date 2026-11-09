package main

import "testing"

func TestPlainText(t *testing.T) {
	cases := map[string]string{
		"Here it is: ![Pairing QR code](/private/tmp/qr.png) done": "Here it is: [Pairing QR code] done",
		"See [the docs](https://example.com) and **this**":         "See the docs and this",
		"## Heading\n- one\n- two":                                 "Heading • one • two",
		"run `go test` now":                                        "run go test now",
		"![](/x.png)":                                              "[picture]",
	}
	for in, want := range cases {
		if got := plainText(in); got != want {
			t.Errorf("plainText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckAnswers(t *testing.T) {
	req := approvalRequest{Tool: "AskUserQuestion", Input: []byte(`{"questions":[{"question":"Q?","multiSelect":true,"options":[{"label":"A"},{"label":"B"}]}]}`)}
	if _, err := checkAnswers(req, map[string]string{"Q?": "A, B"}); err != nil {
		t.Errorf("valid multi answer refused: %v", err)
	}
	if _, err := checkAnswers(req, map[string]string{"Q?": "C"}); err == nil {
		t.Error("an answer that isn't an option was accepted")
	}
	if _, err := checkAnswers(req, map[string]string{}); err == nil {
		t.Error("a missing answer was accepted")
	}
}

func TestUserText(t *testing.T) {
	in := "[User attached one or more files without additional text. Respond using the conversation context and the attached files.]\n\n[Attached image \"image.png\" is saved at: /Users/someone/x/image.png]"
	if got := userText(in); got != "📎 image" {
		t.Errorf("attachment only: got %q", got)
	}
	in = "Look at this\n\n<t3_context version=\"1\">\nname: a.png\n</t3_context>\n\n[Attached image \"a.png\" is saved at: /tmp/a.png]\n[Image: source: /tmp/b.png]"
	if got := userText(in); got != "📎 2 images\nLook at this" {
		t.Errorf("text with images: got %q", got)
	}
	if got := userText("plain [note] here"); got != "plain [note] here" {
		t.Errorf("plain text changed: %q", got)
	}
}
