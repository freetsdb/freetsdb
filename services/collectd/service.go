package collectd // import "github.com/freetsdb/freetsdb/services/collectd"

import (
	"expvar"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/freetsdb/freetsdb"
	"github.com/freetsdb/freetsdb/coordinator"
	"github.com/freetsdb/freetsdb/logger"
	"github.com/freetsdb/freetsdb/models"
	"github.com/freetsdb/freetsdb/services/meta"
	"github.com/freetsdb/freetsdb/tsdb"
	"github.com/kimor79/gollectd"
	"go.uber.org/zap"
)

const leaderWaitTimeout = 30 * time.Second

// statistics gathered by the collectd service.
const (
	statPointsReceived       = "pointsRx"
	statBytesReceived        = "bytesRx"
	statPointsParseFail      = "pointsParseFail"
	statReadFail             = "readFail"
	statBatchesTrasmitted    = "batchesTx"
	statPointsTransmitted    = "pointsTx"
	statBatchesTransmitFail  = "batchesTxFail"
	statDroppedPointsInvalid = "droppedPointsInvalid"
)

// pointsWriter is an internal interface to make testing easier.
type pointsWriter interface {
	WritePoints(p *coordinator.WritePointsRequest) error
}

// metaStore is an internal interface to make testing easier.
type metaClient interface {
	CreateDatabase(name string) (*meta.DatabaseInfo, error)
}

// Service represents a UDP server which receives metrics in collectd's binary
// protocol and stores them in FreeTSDB.
type Service struct {
	Config       *Config
	MetaClient   metaClient
	PointsWriter pointsWriter
	Logger       *zap.Logger

	wg      sync.WaitGroup
	err     chan error
	stop    chan struct{}
	conn    *net.UDPConn
	batcher *tsdb.PointBatcher
	typesdb gollectd.Types
	addr    net.Addr

	// expvar-based stats.
	statMap *expvar.Map
}

// NewService returns a new instance of the collectd service.
func NewService(c Config) *Service {
	s := &Service{
		Config: &c,
		Logger: zap.NewNop(),
		err:    make(chan error),
	}

	return s
}

// Open starts the service.
func (s *Service) Open() error {
	s.Logger.Info("Starting collectd service")

	// Configure expvar monitoring. It's OK to do this even if the service fails to open and
	// should be done before any data could arrive for the service.
	key := strings.Join([]string{"collectd", s.Config.BindAddress}, ":")
	tags := map[string]string{"bind": s.Config.BindAddress}
	s.statMap = freetsdb.NewStatistics(key, "collectd", tags)

	if s.Config.BindAddress == "" {
		return fmt.Errorf("bind address is blank")
	} else if s.Config.Database == "" {
		return fmt.Errorf("database name is blank")
	} else if s.PointsWriter == nil {
		return fmt.Errorf("PointsWriter is nil")
	}

	if _, err := s.MetaClient.CreateDatabase(s.Config.Database); err != nil {
		s.Logger.Info("Failed to ensure target database", logger.Database(s.Config.Database), zap.Error(err))
		return err
	}

	if s.typesdb == nil {
		// Open collectd types.
		typesdb, err := gollectd.TypesDBFile(s.Config.TypesDB)
		if err != nil {
			return fmt.Errorf("Open(): %s", err)
		}
		s.typesdb = typesdb
	}

	// Resolve our address.
	addr, err := net.ResolveUDPAddr("udp", s.Config.BindAddress)
	if err != nil {
		return fmt.Errorf("unable to resolve UDP address: %s", err)
	}
	s.addr = addr

	// Start listening
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("unable to listen on UDP: %s", err)
	}

	if s.Config.ReadBuffer != 0 {
		err = conn.SetReadBuffer(s.Config.ReadBuffer)
		if err != nil {
			return fmt.Errorf("unable to set UDP read buffer to %d: %s",
				s.Config.ReadBuffer, err)
		}
	}
	s.conn = conn

	s.Logger.Info("Listening on UDP", zap.Stringer("addr", conn.LocalAddr()))

	// Start the points batcher.
	s.batcher = tsdb.NewPointBatcher(s.Config.BatchSize, s.Config.BatchPending, time.Duration(s.Config.BatchDuration))
	s.batcher.Start()

	// Create channel and wait group for signalling goroutines to stop.
	s.stop = make(chan struct{})
	s.wg.Add(2)

	// Start goroutines that process collectd packets.
	go s.serve()
	go s.writePoints()

	return nil
}

// Close stops the service.
func (s *Service) Close() error {
	// Close the connection, and wait for the goroutine to exit.
	if s.stop != nil {
		close(s.stop)
	}
	if s.conn != nil {
		s.conn.Close()
	}
	if s.batcher != nil {
		s.batcher.Stop()
	}
	s.wg.Wait()

	// Release all remaining resources.
	s.stop = nil
	s.conn = nil
	s.batcher = nil
	s.Logger.Info("collectd UDP closed")
	return nil
}

// WithLogger sets the logger on the service.
func (s *Service) WithLogger(log *zap.Logger) {
	s.Logger = log.With(zap.String("service", "collectd"))
}

// SetTypes sets collectd types db.
func (s *Service) SetTypes(types string) (err error) {
	s.typesdb, err = gollectd.TypesDB([]byte(types))
	return
}

// Err returns a channel for fatal errors that occur on go routines.
func (s *Service) Err() chan error { return s.err }

// Addr returns the listener's address. Returns nil if listener is closed.
func (s *Service) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Service) serve() {
	defer s.wg.Done()

	// From https://collectd.org/wiki/index.php/Binary_protocol
	//   1024 bytes (payload only, not including UDP / IP headers)
	//   In versions 4.0 through 4.7, the receive buffer has a fixed size
	//   of 1024 bytes. When longer packets are received, the trailing data
	//   is simply ignored. Since version 4.8, the buffer size can be
	//   configured. Version 5.0 will increase the default buffer size to
	//   1452 bytes (the maximum payload size when using UDP/IPv6 over
	//   Ethernet).
	buffer := make([]byte, 1452)

	for {
		select {
		case <-s.stop:
			// We closed the connection, time to go.
			return
		default:
			// Keep processing.
		}

		n, _, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			s.statMap.Add(statReadFail, 1)
			s.Logger.Info("collectd ReadFromUDP error", zap.Error(err))
			continue
		}
		if n > 0 {
			s.statMap.Add(statBytesReceived, int64(n))
			s.handleMessage(buffer[:n])
		}
	}
}

func (s *Service) handleMessage(buffer []byte) {
	packets, err := gollectd.Packets(buffer, s.typesdb)
	if err != nil {
		s.statMap.Add(statPointsParseFail, 1)
		s.Logger.Info("Collectd parse error", zap.Error(err))
		return
	}
	for _, packet := range *packets {
		points := s.UnmarshalCollectd(&packet)
		for _, p := range points {
			s.batcher.In() <- p
		}
		s.statMap.Add(statPointsReceived, int64(len(points)))
	}
}

func (s *Service) writePoints() {
	defer s.wg.Done()

	for {
		select {
		case <-s.stop:
			return
		case batch := <-s.batcher.Out():
			if err := s.PointsWriter.WritePoints(&coordinator.WritePointsRequest{
				Database:         s.Config.Database,
				RetentionPolicy:  s.Config.RetentionPolicy,
				ConsistencyLevel: coordinator.ConsistencyLevelAny,
				Points:           batch,
			}); err == nil {
				s.statMap.Add(statBatchesTrasmitted, 1)
				s.statMap.Add(statPointsTransmitted, int64(len(batch)))
			} else {
				s.Logger.Info("Failed to write point batch",
					logger.Database(s.Config.Database),
					zap.Error(err))
				s.statMap.Add(statBatchesTransmitFail, 1)
			}
		}
	}
}

// Unmarshal translates a collectd packet into FreeTSDB data points.
func (s *Service) UnmarshalCollectd(packet *gollectd.Packet) []models.Point {
	// Prefer high resolution timestamp.
	var timestamp time.Time
	if packet.TimeHR > 0 {
		// TimeHR is "near" nanosecond measurement, but not exactly nanasecond time
		// Since we store time in microseconds, we round here (mostly so tests will work easier)
		sec := packet.TimeHR >> 30
		// Shifting, masking, and dividing by 1 billion to get nanoseconds.
		nsec := ((packet.TimeHR & 0x3FFFFFFF) << 30) / 1000 / 1000 / 1000
		timestamp = time.Unix(int64(sec), int64(nsec)).UTC().Round(time.Microsecond)
	} else {
		// If we don't have high resolution time, fall back to basic unix time
		timestamp = time.Unix(int64(packet.Time), 0).UTC()
	}

	var points []models.Point
	for i := range packet.Values {
		name := fmt.Sprintf("%s_%s", packet.Plugin, packet.Values[i].Name)
		tags := make(map[string]string)
		fields := make(map[string]interface{})

		fields["value"] = packet.Values[i].Value

		if packet.Hostname != "" {
			tags["host"] = packet.Hostname
		}
		if packet.PluginInstance != "" {
			tags["instance"] = packet.PluginInstance
		}
		if packet.Type != "" {
			tags["type"] = packet.Type
		}
		if packet.TypeInstance != "" {
			tags["type_instance"] = packet.TypeInstance
		}
		p, err := models.NewPoint(name, tags, fields, timestamp)
		// Drop invalid points
		if err != nil {
			s.Logger.Info("Dropping point", zap.String("name", name), zap.Error(err))
			s.statMap.Add(statDroppedPointsInvalid, 1)
			continue
		}

		points = append(points, p)
	}
	return points
}

// assert will panic with a given formatted message if the given condition is false.
func assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assert failed: "+msg, v...))
	}
}
