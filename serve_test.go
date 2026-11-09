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
