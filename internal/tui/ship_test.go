package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/yogaagungk/payload/internal/service"
)

func TestModelUpdatesAndView(t *testing.T) {
	model := New()
	run := service.WorkflowRun{ID: 42, Status: "in_progress", HTMLURL: "https://example/42"}
	updated, _ := model.Update(RunMsg{Run: run})
	model = updated.(Model)
	updated, _ = model.Update(UpdateMsg{Run: run, Jobs: []service.Job{{
		ID: 7, Name: "deploy", Status: "in_progress",
		Steps: []service.JobStep{{Number: 1, Name: "Checkout", Status: "completed"}},
	}}})
	model = updated.(Model)
	updated, _ = model.Update(LogMsg{Job: "deploy", Chunk: "line one\nline two\n"})
	model = updated.(Model)

	view := model.View()
	for _, want := range []string{"run 42", "deploy", "Checkout", "line one", "line two"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}

	done, cmd := model.Update(DoneMsg{Run: run, Err: errors.New("failed")})
	model = done.(Model)
	if cmd == nil {
		t.Error("DoneMsg should quit the program")
	}
	if model.Err() == nil || !strings.Contains(model.View(), "failed") {
		t.Error("error not rendered")
	}
}

func TestModelTrimsLogBuffer(t *testing.T) {
	model := New()
	for i := 0; i < 250; i++ {
		updated, _ := model.Update(LogMsg{Job: "job", Chunk: "line\n"})
		model = updated.(Model)
	}
	if len(model.lines) > model.maxLines {
		t.Errorf("lines = %d, max = %d", len(model.lines), model.maxLines)
	}
}

func TestModelQuitKey(t *testing.T) {
	model := New()
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Error("q should quit")
	}
	if updated.(Model).Done() {
		t.Error("a user detach must not mark the run as done")
	}
}
