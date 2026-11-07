// Command payload generates deployment workflows and Dockerfiles.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// version is injected at build time.
var version = "dev"

type app struct {
	stdin       io.Reader
	stdout      io.Writer
	stderr      io.Writer
	interactive bool
}

func newApp(stdin io.Reader, stdout, stderr io.Writer) *app {
	return &app{
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		interactive: isTerminal(stdin),
	}
}

func isTerminal(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func newRootCmd(a *app) *cobra.Command {
	root := &cobra.Command{
		Use:           "payload",
		Short:         "Generate CI/CD workflows and deploy container images",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(a.stdin)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.AddCommand(a.initCmd(), a.shipCmd(), a.versionCmd())
	return root
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the payload version",
		Run: func(_ *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(a.stdout, "payload %s\n", version)
		},
	}
}

func main() {
	a := newApp(os.Stdin, os.Stdout, os.Stderr)
	if err := newRootCmd(a).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "payload:", err)
		os.Exit(1)
	}
}
