package run

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/freetsdb/freetsdb"
	"github.com/freetsdb/freetsdb/coordinator"
	"github.com/freetsdb/freetsdb/logger"
	"github.com/freetsdb/freetsdb/models"
	"github.com/freetsdb/freetsdb/monitor"
	"github.com/freetsdb/freetsdb/services/collectd"
	"github.com/freetsdb/freetsdb/services/continuous_querier"
	"github.com/freetsdb/freetsdb/services/copier"
	"github.com/freetsdb/freetsdb/services/graphite"
	"github.com/freetsdb/freetsdb/services/hh"
	"github.com/freetsdb/freetsdb/services/httpd"
	"github.com/freetsdb/freetsdb/services/meta"
	"github.com/freetsdb/freetsdb/services/opentsdb"
	"github.com/freetsdb/freetsdb/services/precreator"
	"github.com/freetsdb/freetsdb/services/retention"
	"github.com/freetsdb/freetsdb/services/snapshotter"
	"github.com/freetsdb/freetsdb/services/subscriber"
	"github.com/freetsdb/freetsdb/services/udp"
	"github.com/freetsdb/freetsdb/tcp"
	"github.com/freetsdb/freetsdb/tsdb"
	client "github.com/freetsdb/freetsdb/usage-client"
	"go.uber.org/zap"

	// Initialize the engine packages
	_ "github.com/freetsdb/freetsdb/tsdb/engine"
)

var startTime time.Time

const NodeMuxHeader = 9

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

	BindAddress  string
	Listener     net.Listener
	NodeListener net.Listener

	Logger *zap.Logger

	Node    *freetsdb.Node
	NewNode bool

	MetaClient *meta.Client

	TSDBStore     *tsdb.Store
	QueryExecutor *coordinator.QueryExecutor
	PointsWriter  *coordinator.PointsWriter
	ShardWriter   *coordinator.ShardWriter
	HintedHandoff *hh.Service
	Subscriber    *subscriber.Service

	Services []Service

	// These references are required for the tcp muxer.
	CoordinatorService *coordinator.Service
	SnapshotterService *snapshotter.Service
	CopierService      *copier.Service

	Monitor *monitor.Monitor

	// Server reporting and registration
	reportingDisabled bool

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

// NewServer returns a new instance of Server built from a config.
func NewServer(c *Config, buildInfo *BuildInfo) (*Server, error) {
	// We need to ensure that a meta directory always exists even if
	// we don't start the meta store.  node.json is always stored under
	// the meta directory.
	if err := os.MkdirAll(c.Data.Dir, 0777); err != nil {
		return nil, fmt.Errorf("mkdir all: %s", err)
	}

	// 0.10-rc1 and prior would sometimes put the node.json at the root
	// dir which breaks backup/restore and restarting nodes.  This moves
	// the file from the root so it's always under the meta dir.
	oldPath := filepath.Join(filepath.Dir(c.Data.Dir), "node.json")
	newPath := filepath.Join(c.Data.Dir, "node.json")

	if _, err := os.Stat(oldPath); err == nil {
		if err := os.Rename(oldPath, newPath); err != nil {
			return nil, err
		}
	}

	newNode := false
	node, err := freetsdb.LoadNode(c.Data.Dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		newNode = true

		node = freetsdb.NewNode(c.Data.Dir)
	}

	if !c.Data.Enabled {
		return nil, fmt.Errorf("Must run as data node")
	}

	bind := c.BindAddress
	s := &Server{
		buildInfo: *buildInfo,
		err:       make(chan error),
		closing:   make(chan struct{}),

		BindAddress: bind,

		Logger: logger.New(os.Stderr),

		Node:       node,
		NewNode:    newNode,
		MetaClient: meta.NewClient(node),

		Monitor: monitor.New(c.Monitor),

		reportingDisabled: c.ReportingDisabled,

		httpAPIAddr: c.HTTPD.BindAddress,
		httpUseTLS:  c.HTTPD.HTTPSEnabled,
		tcpAddr:     bind,

		config: c,
	}

	if c.Data.Enabled {
		s.TSDBStore = tsdb.NewStore(c.Data.Dir)
		s.TSDBStore.EngineOptions.Config = c.Data

		// Copy TSDB configuration.
		s.TSDBStore.EngineOptions.EngineVersion = c.Data.Engine

		// Set the shard writer
		s.ShardWriter = coordinator.NewShardWriter(time.Duration(c.Coordinator.ShardWriterTimeout),
			c.Coordinator.MaxRemoteWriteConnections)

		// Create the hinted handoff service
		s.HintedHandoff = hh.NewService(c.HintedHandoff, s.ShardWriter, s.MetaClient)
		s.HintedHandoff.Monitor = s.Monitor

		// Create the Subscriber service
		s.Subscriber = subscriber.NewService(c.Subscriber)

		// Initialize points writer.
		s.PointsWriter = coordinator.NewPointsWriter()
		s.PointsWriter.WriteTimeout = time.Duration(c.Coordinator.WriteTimeout)
		s.PointsWriter.TSDBStore = s.TSDBStore
		s.PointsWriter.ShardWriter = s.ShardWriter
		s.PointsWriter.HintedHandoff = s.HintedHandoff
		s.PointsWriter.Subscriber = s.Subscriber
		s.PointsWriter.Node = s.Node

		// Initialize meta executor.
		metaExecutor := coordinator.NewMetaExecutor()
		metaExecutor.MetaClient = s.MetaClient
		metaExecutor.Node = s.Node

		// Initialize query executor.
		s.QueryExecutor = coordinator.NewQueryExecutor()
		s.QueryExecutor.MetaClient = s.MetaClient
		s.QueryExecutor.TSDBStore = s.TSDBStore
		s.QueryExecutor.Monitor = s.Monitor
		s.QueryExecutor.PointsWriter = s.PointsWriter
		s.QueryExecutor.MetaExecutor = metaExecutor

		// Initialize the monitor
		s.Monitor.Version = s.buildInfo.Version
		s.Monitor.Commit = s.buildInfo.Commit
		s.Monitor.Branch = s.buildInfo.Branch
		s.Monitor.BuildTime = s.buildInfo.Time
		s.Monitor.PointsWriter = (*monitorPointsWriter)(s.PointsWriter)
	}

	return s, nil
}

func (s *Server) appendCoordinatorService(c coordinator.Config) {
	srv := coordinator.NewService(c)
	srv.TSDBStore = s.TSDBStore
	srv.MetaClient = s.MetaClient
	s.Services = append(s.Services, srv)
	s.CoordinatorService = srv
}

func (s *Server) appendSnapshotterService() {
	srv := snapshotter.NewService()
	srv.TSDBStore = s.TSDBStore
	srv.MetaClient = s.MetaClient
	srv.Node = s.Node
	s.Services = append(s.Services, srv)
	s.SnapshotterService = srv
}

func (s *Server) appendCopierService() {
	srv := copier.NewService()
	srv.TSDBStore = s.TSDBStore
	s.Services = append(s.Services, srv)
	s.CopierService = srv
}

func (s *Server) appendRetentionPolicyService(c retention.Config) {
	if !c.Enabled {
		return
	}
	srv := retention.NewService(c)
	srv.MetaClient = s.MetaClient
	srv.TSDBStore = s.TSDBStore
	s.Services = append(s.Services, srv)
}

func (s *Server) appendHTTPDService(c httpd.Config) {
	if !c.Enabled {
		return
	}
	srv := httpd.NewService(c)
	srv.Handler.MetaClient = s.MetaClient
	srv.Handler.QueryAuthorizer = meta.NewQueryAuthorizer(s.MetaClient)
	srv.Handler.QueryExecutor = s.QueryExecutor
	srv.Handler.PointsWriter = s.PointsWriter
	srv.Handler.Version = s.buildInfo.Version

	// If a ContinuousQuerier service has been started, attach it.
	for _, srvc := range s.Services {
		if cqsrvc, ok := srvc.(continuous_querier.ContinuousQuerier); ok {
			srv.Handler.ContinuousQuerier = cqsrvc
		}
	}

	s.Services = append(s.Services, srv)
}

func (s *Server) appendCollectdService(c collectd.Config) {
	if !c.Enabled {
		return
	}
	srv := collectd.NewService(c)
	srv.MetaClient = s.MetaClient
	srv.PointsWriter = s.PointsWriter
	s.Services = append(s.Services, srv)
}

func (s *Server) appendOpenTSDBService(c opentsdb.Config) error {
	if !c.Enabled {
		return nil
	}
	srv, err := opentsdb.NewService(c)
	if err != nil {
		return err
	}
	srv.PointsWriter = s.PointsWriter
	srv.MetaClient = s.MetaClient
	s.Services = append(s.Services, srv)
	return nil
}

func (s *Server) appendGraphiteService(c graphite.Config) error {
	if !c.Enabled {
		return nil
	}
	srv, err := graphite.NewService(c)
	if err != nil {
		return err
	}

	srv.PointsWriter = s.PointsWriter
	srv.MetaClient = s.MetaClient
	srv.Monitor = s.Monitor
	s.Services = append(s.Services, srv)
	return nil
}

func (s *Server) appendPrecreatorService(c precreator.Config) error {
	if !c.Enabled {
		return nil
	}
	srv, err := precreator.NewService(c)
	if err != nil {
		return err
	}

	srv.MetaClient = s.MetaClient
	s.Services = append(s.Services, srv)
	return nil
}

func (s *Server) appendUDPService(c udp.Config) {
	if !c.Enabled {
		return
	}
	srv := udp.NewService(c)
	srv.PointsWriter = s.PointsWriter
	srv.MetaClient = s.MetaClient
	s.Services = append(s.Services, srv)
}

func (s *Server) appendContinuousQueryService(c continuous_querier.Config) {
	if !c.Enabled {
		return
	}
	srv := continuous_querier.NewService(c)
	srv.MetaClient = s.MetaClient
	srv.QueryExecutor = s.QueryExecutor
	s.Services = append(s.Services, srv)
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

	s.NodeListener = mux.Listen(NodeMuxHeader)
	go s.nodeService()

	// initialize MetaClient.
	if err = s.initializeMetaClient(); err != nil {
		return err
	}

	if s.TSDBStore != nil {
		// Append services.
		s.appendCoordinatorService(s.config.Coordinator)
		s.appendPrecreatorService(s.config.Precreator)
		s.appendSnapshotterService()
		s.appendCopierService()
		s.appendContinuousQueryService(s.config.ContinuousQuery)
		s.appendHTTPDService(s.config.HTTPD)
		s.appendCollectdService(s.config.Collectd)
		if err := s.appendOpenTSDBService(s.config.OpenTSDB); err != nil {
			return err
		}
		for _, g := range s.config.UDPs {
			s.appendUDPService(g)
		}
		s.appendRetentionPolicyService(s.config.Retention)
		for _, g := range s.config.Graphites {
			if err := s.appendGraphiteService(g); err != nil {
				return err
			}
		}

		s.QueryExecutor.Node = s.Node

		s.Subscriber.MetaClient = s.MetaClient
		s.ShardWriter.MetaClient = s.MetaClient
		s.HintedHandoff.MetaClient = s.MetaClient
		s.Subscriber.MetaClient = s.MetaClient
		s.PointsWriter.MetaClient = s.MetaClient
		s.Monitor.MetaClient = s.MetaClient

		s.CoordinatorService.Listener = mux.Listen(coordinator.MuxHeader)
		s.SnapshotterService.Listener = mux.Listen(snapshotter.MuxHeader)
		s.CopierService.Listener = mux.Listen(copier.MuxHeader)

		s.MetaClient.WithLogger(s.Logger)

		s.TSDBStore.WithLogger(s.Logger)
		if s.config.Data.QueryLogEnabled {
			s.QueryExecutor.WithLogger(s.Logger)
			s.QueryExecutor.MetaExecutor.WithLogger(s.Logger)
		}
		s.PointsWriter.WithLogger(s.Logger)
		s.Subscriber.WithLogger(s.Logger)
		for _, svc := range s.Services {
			svc.WithLogger(s.Logger)
		}
		s.SnapshotterService.WithLogger(s.Logger)
		s.Monitor.WithLogger(s.Logger)

		// Open TSDB store.
		if err := s.TSDBStore.Open(); err != nil {
			// Provide helpful error if user needs to upgrade shards to
			// tsm1.
			if serr, ok := err.(tsdb.ShardError); ok && serr.Err == tsdb.ErrUnknownEngineFormat {
				return freetsdb.ErrUpgradeEngine
			}
			return fmt.Errorf("open tsdb store: %s", err)
		}

		// Open the hinted handoff service
		if err := s.HintedHandoff.Open(); err != nil {
			return fmt.Errorf("open hinted handoff: %s", err)
		}

		// Open the subcriber service
		if err := s.Subscriber.Open(); err != nil {
			return fmt.Errorf("open subscriber: %s", err)
		}

		// Open the points writer service
		if err := s.PointsWriter.Open(); err != nil {
			return fmt.Errorf("open points writer: %s", err)
		}

		// Open the monitor service
		if err := s.Monitor.Open(); err != nil {
			return fmt.Errorf("open monitor: %v", err)
		}

		for _, service := range s.Services {
			if err := service.Open(); err != nil {
				return fmt.Errorf("open service: %s", err)
			}
		}
	}

	// Start the reporting service, if not disabled.
	if !s.reportingDisabled {
		go s.startServerReporting()
	}

	return nil
}

// Close shuts down the meta and data stores and all services.
func (s *Server) Close() error {
	stopProfile()

	// Close the listener first to stop any new connections
	if s.NodeListener != nil {
		s.NodeListener.Close()
	}
	if s.Listener != nil {
		s.Listener.Close()
	}

	// Close services to allow any inflight requests to complete
	// and prevent new requests from being accepted.
	for _, service := range s.Services {
		service.Close()
	}

	if s.Monitor != nil {
		s.Monitor.Close()
	}

	if s.PointsWriter != nil {
		s.PointsWriter.Close()
	}

	if s.HintedHandoff != nil {
		s.HintedHandoff.Close()
	}

	// Close the TSDBStore, no more reads or writes at this point
	if s.TSDBStore != nil {
		s.TSDBStore.Close()
	}

	if s.Subscriber != nil {
		s.Subscriber.Close()
	}

	if s.MetaClient != nil {
		s.MetaClient.Close()
	}

	close(s.closing)
	return nil
}

// startServerReporting starts periodic server reporting.
func (s *Server) startServerReporting() {
	for {
		select {
		case <-s.closing:
			return
		default:
		}
		s.reportServer()
		<-time.After(24 * time.Hour)
	}
}

// reportServer reports anonymous statistics about the system.
func (s *Server) reportServer() {
	dis, err := s.MetaClient.Databases()
	if err != nil {
		log.Printf("failed to retrieve databases for reporting: %s", err.Error())
		return
	}
	numDatabases := len(dis)

	numMeasurements := 0
	numSeries := 0

	// Only needed in the case of a data node
	if s.TSDBStore != nil {
		for _, di := range dis {
			d := s.TSDBStore.DatabaseIndex(di.Name)
			if d == nil {
				// No data in this store for this database.
				continue
			}
			m, s := d.MeasurementSeriesCounts()
			numMeasurements += m
			numSeries += s
		}
	}

	clusterID := s.MetaClient.ClusterID()
	if err != nil {
		log.Printf("failed to retrieve cluster ID for reporting: %s", err.Error())
		return
	}

	cl := client.New("")
	usage := client.Usage{
		Product: "freetsdb",
		Data: []client.UsageData{
			{
				Values: client.Values{
					"os":               runtime.GOOS,
					"arch":             runtime.GOARCH,
					"version":          s.buildInfo.Version,
					"server_id":        fmt.Sprintf("%v", s.Node.ID),
					"cluster_id":       fmt.Sprintf("%v", clusterID),
					"num_series":       numSeries,
					"num_measurements": numMeasurements,
					"num_databases":    numDatabases,
					"uptime":           time.Since(startTime).Seconds(),
				},
			},
		},
	}

	go cl.Save(usage)
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

const RequestClusterJoin = 0x01

type Request struct {
	Type  uint8
	Peers []string
}

func (s *Server) nodeService() error {

	for {
		// Wait for next connection.
		conn, err := s.NodeListener.Accept()
		if err != nil && strings.Contains(err.Error(), "connection closed") {
			log.Printf("DATA node listener closed")
		} else if err != nil {
			log.Printf("Error accepting DATA node request", err.Error())
			continue
		}

		var r Request
		if err := json.NewDecoder(conn).Decode(&r); err != nil {
			log.Printf("Error reading request", err.Error())
		}

		switch r.Type {
		case RequestClusterJoin:
			if !s.NewNode {
				conn.Close()
				continue
			}

			if len(r.Peers) == 0 {
				log.Printf("Invalid MetaServerInfo: empty Peers")
				conn.Close()
				continue
			}

			s.joinCluster(conn, r.Peers)

		default:
			log.Printf("request type unknown: %v", r.Type)
		}
		conn.Close()

	}

	return nil

}

func (s *Server) joinCluster(conn net.Conn, peers []string) {

	metaClient := meta.NewClient(nil)
	metaClient.SetMetaServers(peers)
	if err := metaClient.Open(); err != nil {
		log.Printf("Error open MetaClient", err.Error())
		return
	}

	// if the node ID is > 0 then we need to initialize the metaclient
	if s.Node.ID > 0 {
		metaClient.WaitForDataChanged()
	}

	if s.config.Data.Enabled {
		// If we've already created a data node for our id, we're done
		if _, err := metaClient.DataNode(s.Node.ID); err == nil {
			metaClient.Close()
			return
		}

		n, err := metaClient.CreateDataNode(s.HTTPAddr(), s.TCPAddr())
		for err != nil {
			log.Printf("Unable to create data node. retry in 1s: %s", err.Error())
			time.Sleep(time.Second)
			n, err = s.MetaClient.CreateDataNode(s.HTTPAddr(), s.TCPAddr())
		}
		metaClient.Close()

		s.Node.ID = n.ID
		s.Node.Peers = peers

		if err := s.Node.Save(); err != nil {
			log.Printf("Error save node", err.Error())
			return
		}
		s.NewNode = false

		if err := json.NewEncoder(conn).Encode(n); err != nil {
			log.Printf("Error writing response", err.Error())
		}
	}

}

// initializeMetaClient will set the MetaClient and join the node to the cluster if needed
func (s *Server) initializeMetaClient() error {

	for {
		if len(s.Node.Peers) == 0 {
			time.Sleep(time.Second)
			continue
		}
		s.MetaClient.SetMetaServers(s.Node.Peers)
		break
	}
	s.MetaClient.SetTLS(s.metaUseTLS)

	if err := s.MetaClient.Open(); err != nil {
		return err
	}

	// if the node ID is > 0 then we need to initialize the metaclient
	if s.Node.ID > 0 {
		s.MetaClient.WaitForDataChanged()
	}

	return nil
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

// MetaServers returns the meta node HTTP addresses used by this server.
func (s *Server) MetaServers() []string {
	return s.MetaClient.MetaServers()
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

type tcpaddr struct{ host string }

func (a *tcpaddr) Network() string { return "tcp" }
func (a *tcpaddr) String() string  { return a.host }

// monitorPointsWriter is a wrapper around `cluster.PointsWriter` that helps
// to prevent a circular dependency between the `cluster` and `monitor` packages.
type monitorPointsWriter coordinator.PointsWriter

func (pw *monitorPointsWriter) WritePoints(database, retentionPolicy string, points models.Points) error {
	return (*coordinator.PointsWriter)(pw).WritePoints(&coordinator.WritePointsRequest{
		Database:         database,
		RetentionPolicy:  retentionPolicy,
		ConsistencyLevel: coordinator.ConsistencyLevelOne,
		Points:           points,
	})
}
