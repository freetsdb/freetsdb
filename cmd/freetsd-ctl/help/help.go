package help

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Command displays help for command-line sub-commands.
type Command struct {
	Stdout io.Writer
}

// NewCommand returns a new instance of Command.
func NewCommand() *Command {
	return &Command{
		Stdout: os.Stdout,
	}
}

// Run executes the command.
func (cmd *Command) Run(args ...string) error {
	fmt.Fprintln(cmd.Stdout, strings.TrimSpace(usage))
	return nil
}

const usage = `
Usage:

	freetsd-ctl [[command] [arguments]]

The commands are:

    backup               downloads a snapshot of a data node and saves it to disk
    restore              uses a snapshot of a data node to rebuild a cluster

Use "freetsd-ctl help [command]" for more information about a command.
`
