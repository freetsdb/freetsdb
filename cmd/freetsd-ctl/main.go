package main

import (
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"time"

	"github.com/freetsdb/freetsdb/cmd"
	"github.com/freetsdb/freetsdb/cmd/freetsd-ctl/backup"
	"github.com/freetsdb/freetsdb/cmd/freetsd-ctl/help"
	"github.com/freetsdb/freetsdb/cmd/freetsd-ctl/node"
	"github.com/freetsdb/freetsdb/cmd/freetsd-ctl/restore"
)

// These variables are populated via the Go linker.
var (
	version string
	commit  string
	branch  string
)

func init() {
	// If commit, branch, or build time are not set, make that clear.
	if version == "" {
		version = "unknown"
	}
	if commit == "" {
		commit = "unknown"
	}
	if branch == "" {
		branch = "unknown"
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())

	m := NewMain()
	if err := m.Run(os.Args[1:]...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Main represents the program execution.
type Main struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// NewMain return a new instance of Main.
func NewMain() *Main {
	return &Main{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// Run determines and runs the command specified by the CLI args.
func (m *Main) Run(args ...string) error {
	name, args := cmd.ParseCommandName(args)

	// Extract name from args.
	switch name {
	case "", "help":
		if err := help.NewCommand().Run(args...); err != nil {
			return fmt.Errorf("help: %s", err)
		}

	case "backup":
		name := backup.NewCommand()
		if err := name.Run(args...); err != nil {
			return fmt.Errorf("backup: %s", err)
		}
	case "restore":
		name := restore.NewCommand()
		if err := name.Run(args...); err != nil {
			return fmt.Errorf("restore: %s", err)
		}
	case "add-meta", "remove-meta", "add-data", "remove-data", "show":
		cmd := node.NewCommand(name)
		if err := cmd.Run(args...); err != nil {
			return fmt.Errorf("%s: %s", name, err)
		}
	default:
		return fmt.Errorf(`unknown command "%s"`+"\n"+`Run 'freetsd-ctl help' for usage`+"\n\n", name)
	}

	return nil
}

// VersionCommand represents the command executed by "freetsd-ctl version".
type VersionCommand struct {
	Stdout io.Writer
	Stderr io.Writer
}

// NewVersionCommand return a new instance of VersionCommand.
func NewVersionCommand() *VersionCommand {
	return &VersionCommand{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// Run prints the current version and commit info.
func (cmd *VersionCommand) Run(args ...string) error {
	// Parse flags in case -h is specified.
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(cmd.Stderr, versionUsage) }
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Print version info.
	fmt.Fprintf(cmd.Stdout, "FreeTSDB v%s (git: %s %s)\n", version, branch, commit)

	return nil
}

var versionUsage = `Displays the FreeTSDB version, build branch and git commit hash.

Usage: freetsd-ctl version
`
