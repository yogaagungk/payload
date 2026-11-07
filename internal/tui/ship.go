// Package tui renders live deploy status with bubbletea.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/yogaagungk/payload/internal/service"
)

// RunMsg announces a dispatched run.
type RunMsg struct{ Run service.WorkflowRun }

// UpdateMsg carries the latest run and job state.
type UpdateMsg struct {
	Run  service.WorkflowRun
	Jobs []service.Job
}

// LogMsg carries a log chunk for one job.
type LogMsg struct {
	Job   string
	Chunk string
}

// DoneMsg terminates the program.
type DoneMsg struct {
	Run service.WorkflowRun
	Err error
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	failStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	activeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
)

// Model is the bubbletea model for a ship run.
type Model struct {
	run      *service.WorkflowRun
	jobs     []service.Job
	lines    []string
	runErr   error
	done     bool
	maxLines int
}

// New builds an empty model.
func New() Model {
	return Model{maxLines: 200}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd { return nil }

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch value := msg.(type) {
	case RunMsg:
		run := value.Run
		m.run = &run
	case UpdateMsg:
		run := value.Run
		m.run = &run
		m.jobs = value.Jobs
	case LogMsg:
		for _, line := range strings.Split(strings.TrimRight(service.SanitizeLog(value.Chunk), "\n"), "\n") {
			m.lines = append(m.lines, fmt.Sprintf("%s %s", dimStyle.Render("["+value.Job+"]"), line))
		}
		if len(m.lines) > m.maxLines {
			m.lines = m.lines[len(m.lines)-m.maxLines:]
		}
	case DoneMsg:
		run := value.Run
		m.run = &run
		m.runErr = value.Err
		m.done = true
		return m, tea.Quit
	case tea.KeyMsg:
		switch value.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	}
	return m, nil
}

// View implements tea.Model.
func (m Model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("payload ship"))
	b.WriteString("\n")
	if m.run != nil {
		status := m.run.Status
		if m.run.Conclusion != "" {
			status = m.run.Conclusion
		}
		fmt.Fprintf(&b, "run %d  %s  %s\n", m.run.ID, statusStyle(status), m.run.HTMLURL)
	}
	b.WriteString("\n")
	for _, job := range m.jobs {
		status := job.Status
		if job.Conclusion != "" {
			status = job.Conclusion
		}
		fmt.Fprintf(&b, "%s %s\n", statusStyle(status), job.Name)
		for _, step := range job.Steps {
			stepStatus := step.Status
			if step.Conclusion != "" {
				stepStatus = step.Conclusion
			}
			fmt.Fprintf(&b, "  %s %s\n", statusStyle(stepStatus), step.Name)
		}
	}
	if len(m.lines) > 0 {
		b.WriteString(dimStyle.Render(strings.Repeat("─", 40)))
		b.WriteString("\n")
		b.WriteString(strings.Join(m.lines, "\n"))
		b.WriteString("\n")
	}
	if m.done {
		if m.runErr != nil {
			b.WriteString(failStyle.Render("failed: " + m.runErr.Error()))
		} else {
			b.WriteString(okStyle.Render("deployed successfully"))
		}
		b.WriteString("\n")
	} else {
		b.WriteString(dimStyle.Render("q to detach (the run keeps going)"))
	}
	return b.String()
}

// Err returns the final error after the program exits.
func (m Model) Err() error { return m.runErr }

// Done reports whether the run finished (as opposed to a user detach).
func (m Model) Done() bool { return m.done }

// Run returns the final run state.
func (m Model) Run() *service.WorkflowRun { return m.run }

func statusStyle(status string) string {
	switch status {
	case "success", "completed":
		return okStyle.Render(status)
	case "failure", "cancelled", "timed_out", "action_required":
		return failStyle.Render(status)
	case "in_progress", "queued", "waiting", "pending":
		return activeStyle.Render(status)
	default:
		return status
	}
}

// ProgramReporter forwards ship events to a bubbletea program.
type ProgramReporter struct {
	Program *tea.Program
}

// RunStarted implements service.Reporter.
func (r ProgramReporter) RunStarted(run service.WorkflowRun) {
	r.Program.Send(RunMsg{Run: run})
}

// Update implements service.Reporter.
func (r ProgramReporter) Update(run service.WorkflowRun, jobs []service.Job) {
	r.Program.Send(UpdateMsg{Run: run, Jobs: jobs})
}

// Log implements service.Reporter.
func (r ProgramReporter) Log(job, chunk string) {
	r.Program.Send(LogMsg{Job: job, Chunk: chunk})
}

// Warn implements service.Warner.
func (r ProgramReporter) Warn(message string) {
	r.Program.Send(LogMsg{Job: "payload", Chunk: "warning: " + message + "\n"})
}
