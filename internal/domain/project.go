// Package domain holds payload's project models.
package domain

// Stack is a supported application stack.
type Stack string

// Supported stacks.
const (
	StackGo     Stack = "go"
	StackNode   Stack = "node"
	StackPython Stack = "python"
	StackStatic Stack = "static"
)

// AllStacks lists the wizard options in display order.
var AllStacks = []Stack{StackGo, StackNode, StackPython, StackStatic}

// Valid reports whether s is a supported stack.
func (s Stack) Valid() bool {
	switch s {
	case StackGo, StackNode, StackPython, StackStatic:
		return true
	default:
		return false
	}
}

// DefaultPort is the host port suggested for a stack.
func (s Stack) DefaultPort() int {
	if s == StackStatic {
		return 8080
	}
	return 3000
}

// ContainerPort is the port the container listens on. Static sites use nginx
// on port 80; application stacks listen on the configured port.
func (s Stack) ContainerPort(hostPort int) int {
	if s == StackStatic {
		return 80
	}
	return hostPort
}

// ProjectOptions describes one generated deployment.
type ProjectOptions struct {
	AppName    string
	Stack      Stack
	Port       int
	HealthPath string
}
