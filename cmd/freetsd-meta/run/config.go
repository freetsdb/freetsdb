package run

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/freetsdb/freetsdb/logger"
	"github.com/freetsdb/freetsdb/pkg/tlsconfig"
	"github.com/freetsdb/freetsdb/services/meta"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

const (
	// DefaultBindAddress is the default address for raft, snapshot, etc..
	DefaultBindAddress = ":8088"

	// DefaultHostname is the default hostname used if we are unable to determine
	// the hostname from the system
	DefaultHostname = "localhost"
)

// Config represents the configuration format for the freetsd-meta binary.
type Config struct {
	Meta    *meta.Config  `toml:"meta"`
	Logging logger.Config `toml:"logging"`

	// BindAddress is the address that all TCP services use (Raft, Snapshot, etc.)
	BindAddress string `toml:"bind-address"`

	// Hostname is the hostname portion to use when registering local
	// addresses.  This hostname must be resolvable from other nodes.
	Hostname string `toml:"hostname"`

	// TLS provides configuration options for all https endpoints.
	TLS tlsconfig.Config `toml:"tls"`
}

// NewConfig returns an instance of Config with reasonable defaults.
func NewConfig() *Config {
	c := &Config{}
	c.Meta = meta.NewConfig()
	c.Logging = logger.NewConfig()

	c.BindAddress = DefaultBindAddress

	return c
}

// NewDemoConfig returns the config that runs when no config is specified.
func NewDemoConfig() (*Config, error) {
	c := NewConfig()

	var homeDir string
	// By default, store meta and data files in current users home directory
	u, err := user.Current()
	if err == nil {
		homeDir = u.HomeDir
	} else if os.Getenv("HOME") != "" {
		homeDir = os.Getenv("HOME")
	} else {
		return nil, fmt.Errorf("failed to determine current user for storage")
	}

	c.Meta.Dir = filepath.Join(homeDir, ".freetsdb/meta")

	return c, nil
}

// FromTomlFile loads the config from a TOML file.
func (c *Config) FromTomlFile(fpath string) error {
	bs, err := ioutil.ReadFile(fpath)
	if err != nil {
		return err
	}

	// Handle any potential Byte-Order-Marks that may be in the config file.
	// This is for Windows compatibility only.
	bom := unicode.BOMOverride(transform.Nop)
	bs, _, err = transform.Bytes(bom, bs)
	if err != nil {
		return err
	}
	return c.FromToml(string(bs))
}

// FromToml loads the config from TOML.
func (c *Config) FromToml(input string) error {
	_, err := toml.Decode(input, c)
	return err
}

// Validate returns an error if the config is invalid.
func (c *Config) Validate() error {

	if err := c.Meta.Validate(); err != nil {
		return err
	}

	if err := c.TLS.Validate(); err != nil {
		return err
	}

	return nil
}
