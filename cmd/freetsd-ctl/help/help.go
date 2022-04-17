// Package help is the help subcommand of the freetsd-ctl command.
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
Configure and start an FreeTSDB server.

Usage: freetsd-ctl [[command] [arguments]]

The commands are:

    backup               downloads a snapshot of a data node and saves it to disk
    config               display the default configuration
    help                 display this help message
    restore              uses a snapshot of a data node to rebuild a cluster
    show                 show node status
    version              displays the FreeTSDB version

"run" is the default command.

Use "freetsd-ctl [command] -help" for more information about a command.
`
