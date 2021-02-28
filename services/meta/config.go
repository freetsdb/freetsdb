package meta

import (
	"errors"
	"net"
	"time"

	"github.com/freetsdb/freetsdb/monitor/diagnostics"
	"github.com/freetsdb/freetsdb/toml"
)

const (
	// DefaultEnabled is the default state for the meta service to run
	DefaultEnabled = true

	// DefaultHostname is the default hostname if one is not provided.
	DefaultHostname = "localhost"

	// DefaultRaftBindAddress is the default address to bind to.
	DefaultRaftBindAddress = ":8088"

	// DefaultHTTPBindAddress is the default address to bind the API to.
	DefaultHTTPBindAddress = ":8091"

	// DefaultHeartbeatTimeout is the default heartbeat timeout for the store.
	DefaultHeartbeatTimeout = 1000 * time.Millisecond

	// DefaultElectionTimeout is the default election timeout for the store.
	DefaultElectionTimeout = 1000 * time.Millisecond

	// DefaultLeaderLeaseTimeout is the default leader lease for the store.
	DefaultLeaderLeaseTimeout = 500 * time.Millisecond

	// DefaultCommitTimeout is the default commit timeout for the store.
	DefaultCommitTimeout = 50 * time.Millisecond

	// DefaultRaftPromotionEnabled is the default for auto promoting a node to a raft node when needed
	DefaultRaftPromotionEnabled = true

	// DefaultLeaseDuration is the default duration for leases.
	DefaultLeaseDuration = 60 * time.Second

	// DefaultLoggingEnabled determines if log messages are printed for the meta service
	DefaultLoggingEnabled = true
)

// Config represents the meta configuration.
type Config struct {
	Dir string `toml:"dir"`

	// RemoteHostname is the hostname portion to use when registering meta node
	// addresses.  This hostname must be resolvable from other nodes.
	RemoteHostname string `toml:"-"`

	// this is deprecated. Should use the address from run/config.go
	BindAddress string `toml:"bind-address"`

	// HTTPBindAddress is the bind address for the metaservice HTTP API
	HTTPBindAddress  string `toml:"http-bind-address"`
	HTTPSEnabled     bool   `toml:"https-enabled"`
	HTTPSCertificate string `toml:"https-certificate"`

	RetentionAutoCreate  bool          `toml:"retention-autocreate"`
	ElectionTimeout      toml.Duration `toml:"election-timeout"`
	HeartbeatTimeout     toml.Duration `toml:"heartbeat-timeout"`
	LeaderLeaseTimeout   toml.Duration `toml:"leader-lease-timeout"`
	CommitTimeout        toml.Duration `toml:"commit-timeout"`
	ClusterTracing       bool          `toml:"cluster-tracing"`
	RaftPromotionEnabled bool          `toml:"raft-promotion-enabled"`
	LoggingEnabled       bool          `toml:"logging-enabled"`
	PprofEnabled         bool          `toml:"pprof-enabled"`

	LeaseDuration toml.Duration `toml:"lease-duration"`
}

// NewConfig builds a new configuration with default values.
func NewConfig() *Config {
	return &Config{
		BindAddress:          DefaultRaftBindAddress,
		HTTPBindAddress:      DefaultHTTPBindAddress,
		RetentionAutoCreate:  true,
		ElectionTimeout:      toml.Duration(DefaultElectionTimeout),
		HeartbeatTimeout:     toml.Duration(DefaultHeartbeatTimeout),
		LeaderLeaseTimeout:   toml.Duration(DefaultLeaderLeaseTimeout),
		CommitTimeout:        toml.Duration(DefaultCommitTimeout),
		RaftPromotionEnabled: DefaultRaftPromotionEnabled,
		LeaseDuration:        toml.Duration(DefaultLeaseDuration),
		LoggingEnabled:       DefaultLoggingEnabled,
	}

}

// Validate returns an error if the config is invalid.
func (c *Config) Validate() error {
	if c.Dir == "" {
		return errors.New("Meta.Dir must be specified")
	}
	return nil
}

// Diagnostics returns a diagnostics representation of a subset of the Config.
func (c *Config) Diagnostics() (*diagnostics.Diagnostics, error) {
	return diagnostics.RowFromMap(map[string]interface{}{
		"dir": c.Dir,
	}), nil
}

func (c *Config) defaultHost(addr string) string {
	address, err := DefaultHost(DefaultHostname, addr)
	if nil != err {
		return addr
	}
	return address
}

func DefaultHost(hostname, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}

	if host == "" || host == "0.0.0.0" || host == "::" {
		return net.JoinHostPort(hostname, port), nil
	}
	return addr, nil
}
