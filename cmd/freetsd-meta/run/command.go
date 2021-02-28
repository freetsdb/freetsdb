// Package run is the run (default) subcommand for the freetsd-meta command.
package run

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/freetsdb/freetsdb/logger"
	"go.uber.org/zap"
)

const logo = `
 ______                    _____  _____ ______ ______ 
 |  ___|                  |_   _|/  ___||  _  \| ___ \
 | |_    _ __   ___   ___   | |  \ \--. | | | || |_/ /
 |  _|  | '__| / _ \ / _ \  | |   \--. \| | | || ___ \
 | |    | |   |  __/|  __/  | |  /\__/ /| |/ / | |_/ /
 \_|    |_|    \___| \___|  |_|  \____/ |___/  |____/ 

`

// Command represents the command executed by "freetsd-meta run".
type Command struct {
	Version   string
	Branch    string
	Commit    string
	BuildTime string

	closing chan struct{}
	pidfile string
	Closed  chan struct{}

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Logger *zap.Logger

	Server *Server
}

// NewCommand return a new instance of Command.
func NewCommand() *Command {
	return &Command{
		closing: make(chan struct{}),
		Closed:  make(chan struct{}),
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Logger:  zap.NewNop(),
	}
}

// Run parses the config from args and runs the server.
func (cmd *Command) Run(args ...string) error {
	// Parse the command line flags.
	options, err := cmd.ParseFlags(args...)
	if err != nil {
		return err
	}

	config, err := cmd.ParseConfig(options.GetConfigPath())
	if err != nil {
		return fmt.Errorf("parse config: %s", err)
	}

	if options.Hostname != "" {
		config.Hostname = options.Hostname
	}

	// Propogate the top-level hostname down to dependendent configs
	config.Meta.RemoteHostname = config.Hostname

	// Validate the configuration.
	if err := config.Validate(); err != nil {
		return fmt.Errorf("%s. To generate a valid configuration file run `freetsd-meta config > freetsdb.generated.conf`", err)
	}

	var logErr error
	if cmd.Logger, logErr = config.Logging.New(cmd.Stderr); logErr != nil {
		// assign the default logger
		cmd.Logger = logger.New(cmd.Stderr)
	}

	// Print sweet FreeTSDB logo.
	if !config.Logging.SuppressLogo && logger.IsTerminal(cmd.Stdout) {
		fmt.Fprint(cmd.Stdout, logo)
	}

	// Mark start-up in log.
	cmd.Logger.Info("FreeTSDB Meta server starting",
		zap.String("version", cmd.Version),
		zap.String("branch", cmd.Branch),
		zap.String("commit", cmd.Commit))
	cmd.Logger.Info("Go runtime",
		zap.String("version", runtime.Version()),
		zap.Int("maxprocs", runtime.GOMAXPROCS(0)))

	// If there was an error on startup when creating the logger, output it now.
	if logErr != nil {
		cmd.Logger.Error("Unable to configure logger", zap.Error(logErr))
	}

	// Write the PID file.
	if err := cmd.writePIDFile(options.PIDFile); err != nil {
		return fmt.Errorf("write pid file: %s", err)
	}
	cmd.pidfile = options.PIDFile

	// Create server from config and start it.
	buildInfo := &BuildInfo{
		Version: cmd.Version,
		Commit:  cmd.Commit,
		Branch:  cmd.Branch,
		Time:    cmd.BuildTime,
	}
	s, err := NewServer(config, buildInfo)
	if err != nil {
		return fmt.Errorf("create server: %s", err)
	}
	s.Logger = cmd.Logger
	s.CPUProfile = options.CPUProfile
	s.MemProfile = options.MemProfile
	if err := s.Open(); err != nil {
		return fmt.Errorf("open server: %s", err)
	}
	cmd.Server = s

	// Begin monitoring the server's error channel.
	go cmd.monitorServerErrors()

	return nil
}

// Close shuts down the server.
func (cmd *Command) Close() error {
	defer close(cmd.Closed)
	defer cmd.removePIDFile()
	close(cmd.closing)
	if cmd.Server != nil {
		return cmd.Server.Close()
	}
	return nil
}

func (cmd *Command) monitorServerErrors() {
	logger := log.New(cmd.Stderr, "", log.LstdFlags)
	for {
		select {
		case err := <-cmd.Server.Err():
			logger.Println(err)
		case <-cmd.closing:
			return
		}
	}
}

func (cmd *Command) removePIDFile() {
	if cmd.pidfile != "" {
		if err := os.Remove(cmd.pidfile); err != nil {
			cmd.Logger.Error("Unable to remove pidfile", zap.Error(err))
		}
	}
}

// ParseFlags parses the command line flags from args and returns an options set.
func (cmd *Command) ParseFlags(args ...string) (Options, error) {
	var options Options
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	fs.StringVar(&options.ConfigPath, "config", "", "")
	fs.StringVar(&options.PIDFile, "pidfile", "", "")
	fs.StringVar(&options.Hostname, "hostname", "", "")
	fs.StringVar(&options.CPUProfile, "cpuprofile", "", "")
	fs.StringVar(&options.MemProfile, "memprofile", "", "")
	fs.Usage = func() { fmt.Fprintln(cmd.Stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}

	return options, nil
}

// writePIDFile writes the process ID to path.
func (cmd *Command) writePIDFile(path string) error {
	// Ignore if path is not set.
	if path == "" {
		return nil
	}

	// Ensure the required directory structure exists.
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		return fmt.Errorf("mkdir: %s", err)
	}

	// Retrieve the PID and write it.
	pid := strconv.Itoa(os.Getpid())
	if err := ioutil.WriteFile(path, []byte(pid), 0666); err != nil {
		return fmt.Errorf("write file: %s", err)
	}

	return nil
}

// ParseConfig parses the config at path.
// It returns a demo configuration if path is blank.
func (cmd *Command) ParseConfig(path string) (*Config, error) {
	// Use demo configuration if no config path is specified.
	if path == "" {
		cmd.Logger.Info("No configuration provided, using default settings")
		return NewDemoConfig()
	}

	cmd.Logger.Info("Loading configuration file", zap.String("path", path))

	config := NewConfig()
	if err := config.FromTomlFile(path); err != nil {
		return nil, err
	}

	return config, nil
}

const usage = `usage: freetsd-meta [flags]

FreeTSDB Meta is a raft-based meta service for FreeTSDB.

Options:
	-config <path>       Set the path to the configuration file.

	-single-server       Start the server in single server mode.  This
	                     will trigger an election.  If you are starting
	                     multiple nodes to form a cluster, this option
	                     should not be used.

	-hostname <name>     Override the hostname, the 'hostname' configuration
	                     option will be overridden.

	-pidfile <path>      Write process ID to a file.

	-cpuprofile <path>   Write CPU profiling information to a file.

	-memprofile <path>   Write memory usage information to a file.
`

// Options represents the command line options that can be parsed.
type Options struct {
	ConfigPath string
	PIDFile    string
	Hostname   string
	CPUProfile string
	MemProfile string
}

// GetConfigPath returns the config path from the options.
// It will return a path by searching in this order:
//   1. The CLI option in ConfigPath
//   2. The environment variable FREETSDB_CONFIG_PATH
//   3. The first freetsdb.conf file on the path:
//        - ~/.freetsdb
//        - /etc/freetsdb
func (opt *Options) GetConfigPath() string {
	if opt.ConfigPath != "" {
		if opt.ConfigPath == os.DevNull {
			return ""
		}
		return opt.ConfigPath
	}

	for _, path := range []string{
		os.ExpandEnv("${HOME}/.freetsdb/freetsdb-meta.conf"),
		"/etc/freetsdb/freetsdb-meta.conf",
	} {
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}
