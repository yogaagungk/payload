package domain

import "testing"

func TestStackProperties(t *testing.T) {
	for _, stack := range AllStacks {
		if !stack.Valid() {
			t.Errorf("%q not valid", stack)
		}
	}
	if Stack("rust").Valid() {
		t.Error("rust reported valid")
	}
	if StackStatic.DefaultPort() != 8080 || StackGo.DefaultPort() != 3000 {
		t.Error("unexpected default ports")
	}
	if StackStatic.ContainerPort(8080) != 80 {
		t.Error("static container port must be 80")
	}
	if StackNode.ContainerPort(3000) != 3000 {
		t.Error("node container port must follow the host port")
	}
}
