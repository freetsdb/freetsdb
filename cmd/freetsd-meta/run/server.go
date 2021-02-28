package run

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"time"

	"github.com/freetsdb/freetsdb"
	"github.com/freetsdb/freetsdb/logger"
	"github.com/freetsdb/freetsdb/services/meta"
	"github.com/freetsdb/freetsdb/tcp"
	"go.uber.org/zap"
)

var startTime time.Time

func init() {
	startTime = time.Now().UTC()
}

// BuildInfo represents the build details for the server code.
type BuildInfo struct {
	Version string
	Commit  string
	Branch  string
	Time    string
}

// Server represents a container for the metadata and storage data and services.
// It is built using a Config and it manages the startup and shutdown of all
// services in the proper order.
type Server struct {
	buildInfo BuildInfo

	err     chan error
	closing chan struct{}

	BindAddress string
	Listener    net.Listener

	Logger *zap.Logger

	Node *freetsdb.Node

	MetaService *meta.Service

	// Profiling
	CPUProfile string
	MemProfile string

	// metaUseTLS specifies if we should use a TLS connection to the meta servers
	metaUseTLS bool

	// httpAPIAddr is the host:port combination for the main HTTP API for querying and writing data
	httpAPIAddr string

	// httpUseTLS specifies if we should use a TLS connection to the http servers
	httpUseTLS bool

	// tcpAddr is the host:port combination for the TCP listener that services mux onto
	tcpAddr string

	config *Config
}

// updateTLSConfig stores with into the tls config pointed at by into but only if with is not nil
// and into is nil. Think of it as setting the default value.
func updateTLSConfig(into **tls.Config, with *tls.Config) {
	if with != nil && into != nil && *into == nil {
		*into = with
	}
}

// NewServer returns a new instance of Server built from a config.
func NewServer(c *Config, buildInfo *BuildInfo) (*Server, error) {

	// We need to ensure that a meta directory always exists even if
	// we don't start the meta store.  node.json is always stored under
	// the meta directory.
	if err := os.MkdirAll(c.Meta.Dir, 0777); err != nil {
		return nil, fmt.Errorf("mkdir all: %s", err)
	}

	// 0.10-rc1 and prior would sometimes put the node.json at the root
	// dir which breaks backup/restore and restarting nodes.  This moves
	// the file from the root so it's always under the meta dir.
	oldPath := filepath.Join(filepath.Dir(c.Meta.Dir), "node.json")
	newPath := filepath.Join(c.Meta.Dir, "node.json")

	if _, err := os.Stat(oldPath); err == nil {
		if err := os.Rename(oldPath, newPath); err != nil {
			return nil, err
		}
	}

	node, err := freetsdb.LoadNode(c.Meta.Dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}

		node = freetsdb.NewNode(c.Meta.Dir)
	}

	// In 0.10.0 bind-address got moved to the top level. Check
	// The old location to keep things backwards compatible
	bind := c.BindAddress
	if c.Meta.BindAddress != "" {
		bind = c.Meta.BindAddress
	}

	s := &Server{
		buildInfo: *buildInfo,
		err:       make(chan error),
		closing:   make(chan struct{}),

		BindAddress: bind,

		Logger: logger.New(os.Stderr),

		Node: node,

		metaUseTLS: c.Meta.HTTPSEnabled,

		tcpAddr: bind,

		config: c,
	}

	s.MetaService = meta.NewService(c.Meta)
	s.MetaService.Version = s.buildInfo.Version
	s.MetaService.Node = s.Node

	return s, nil
}

// Err returns an error channel that multiplexes all out of band errors received from all services.
func (s *Server) Err() <-chan error { return s.err }

// Open opens the meta and data store and all services.
func (s *Server) Open() error {
	// Start profiling, if set.
	startProfile(s.CPUProfile, s.MemProfile)

	// Open shared TCP connection.
	ln, err := net.Listen("tcp", s.BindAddress)
	if err != nil {
		return fmt.Errorf("listen: %s", err)
	}
	s.Listener = ln

	// Multiplex listener.
	mux := tcp.NewMux()
	go mux.Serve(ln)

	if s.MetaService != nil {
		s.MetaService.RaftListener = mux.Listen(meta.MuxHeader)

		// Configure logging for all services and clients.
		if s.config.Meta.LoggingEnabled {
			s.MetaService.WithLogger(s.Logger)
		}

		// Open meta service.
		if err := s.MetaService.Open(); err != nil {
			return fmt.Errorf("open meta service: %s", err)
		}

		go s.monitorErrorChan(s.MetaService.Err())
	}

	return nil
}

// Close shuts down the meta and data stores and all services.
func (s *Server) Close() error {
	stopProfile()

	// Close the listener first to stop any new connections
	if s.Listener != nil {
		s.Listener.Close()
	}

	if s.MetaService != nil {
		s.MetaService.Close()
	}

	close(s.closing)
	return nil
}

// monitorErrorChan reads an error channel and resends it through the server.
func (s *Server) monitorErrorChan(ch <-chan error) {
	for {
		select {
		case err, ok := <-ch:
			if !ok {
				return
			}
			s.err <- err
		case <-s.closing:
			return
		}
	}
}

// HTTPAddr returns the HTTP address used by other nodes for HTTP queries and writes.
func (s *Server) HTTPAddr() string {
	return s.remoteAddr(s.httpAPIAddr)
}

// TCPAddr returns the TCP address used by other nodes for cluster communication.
func (s *Server) TCPAddr() string {
	return s.remoteAddr(s.tcpAddr)
}

func (s *Server) remoteAddr(addr string) string {
	hostname := s.config.Hostname
	if hostname == "" {
		hostname = meta.DefaultHostname
	}
	remote, err := meta.DefaultHost(hostname, addr)
	if err != nil {
		return addr
	}
	return remote
}

// Service represents a service attached to the server.
type Service interface {
	WithLogger(log *zap.Logger)
	Open() error
	Close() error
}

// prof stores the file locations of active profiles.
var prof struct {
	cpu *os.File
	mem *os.File
}

// StartProfile initializes the cpu and memory profile, if specified.
func startProfile(cpuprofile, memprofile string) {
	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			log.Fatalf("cpuprofile: %v", err)
		}
		log.Printf("writing CPU profile to: %s\n", cpuprofile)
		prof.cpu = f
		pprof.StartCPUProfile(prof.cpu)
	}

	if memprofile != "" {
		f, err := os.Create(memprofile)
		if err != nil {
			log.Fatalf("memprofile: %v", err)
		}
		log.Printf("writing mem profile to: %s\n", memprofile)
		prof.mem = f
		runtime.MemProfileRate = 4096
	}

}

// StopProfile closes the cpu and memory profiles if they are running.
func stopProfile() {
	if prof.cpu != nil {
		pprof.StopCPUProfile()
		prof.cpu.Close()
		log.Println("CPU profile stopped")
	}
	if prof.mem != nil {
		pprof.Lookup("heap").WriteTo(prof.mem, 0)
		prof.mem.Close()
		log.Println("mem profile stopped")
	}
}
