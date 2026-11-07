package service

import (
	"strings"
	"testing"
)

func TestSanitizeLog(t *testing.T) {
	input := "safe\tok\n\x1b[31mred\x1b[0m\n\x1b]0;window title\x07after\x00end\r"
	got := SanitizeLog(input)
	want := "safe\tok\n\x1b[31mred\x1b[0m\nafterend"
	if got != want {
		t.Fatalf("SanitizeLog = %q, want %q", got, want)
	}
	if strings.Contains(got, "window title") {
		t.Error("OSC title sequence was not stripped")
	}
}
