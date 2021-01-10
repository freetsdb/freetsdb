package udp // import "github.com/freetsdb/freetsdb/services/udp"

import (
	"errors"
	"expvar"
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
	"go.uber.org/zap"
)

const (
	// Arbitrary, testing indicated that this doesn't typically get over 10
	parserChanLen = 1000
)

// statistics gathered by the UDP package.
const (
	statPointsReceived      = "pointsRx"
	statBytesReceived       = "bytesRx"
	statPointsParseFail     = "pointsParseFail"
	statReadFail            = "readFail"
	statBatchesTrasmitted   = "batchesTx"
	statPointsTransmitted   = "pointsTx"
	statBatchesTransmitFail = "batchesTxFail"
)

//
// Service represents here an UDP service
// that will listen for incoming packets
// formatted with the inline protocol
//
type Service struct {
	conn *net.UDPConn
	addr *net.UDPAddr
	wg   sync.WaitGroup
	done chan struct{}

	parserChan chan []byte
	batcher    *tsdb.PointBatcher
	config     Config

	PointsWriter interface {
		WritePoints(p *coordinator.WritePointsRequest) error
	}

	MetaClient interface {
		CreateDatabase(name string) (*meta.DatabaseInfo, error)
	}

	Logger  *zap.Logger
	statMap *expvar.Map
}

// NewService returns a new instance of Service.
func NewService(c Config) *Service {
	d := *c.WithDefaults()
	return &Service{
		config:     d,
		done:       make(chan struct{}),
		parserChan: make(chan []byte, parserChanLen),
		batcher:    tsdb.NewPointBatcher(d.BatchSize, d.BatchPending, time.Duration(d.BatchTimeout)),
		Logger:     zap.NewNop(),
	}
}

// Open starts the service
func (s *Service) Open() (err error) {
	// Configure expvar monitoring. It's OK to do this even if the service fails to open and
	// should be done before any data could arrive for the service.
	key := strings.Join([]string{"udp", s.config.BindAddress}, ":")
	tags := map[string]string{"bind": s.config.BindAddress}
	s.statMap = freetsdb.NewStatistics(key, "udp", tags)

	if s.config.BindAddress == "" {
		return errors.New("bind address has to be specified in config")
	}
	if s.config.Database == "" {
		return errors.New("database has to be specified in config")
	}

	if _, err := s.MetaClient.CreateDatabase(s.config.Database); err != nil {
		return errors.New("Failed to ensure target database exists")
	}

	s.addr, err = net.ResolveUDPAddr("udp", s.config.BindAddress)
	if err != nil {
		s.Logger.Info("Failed to resolve UDP address",
			zap.String("addr", s.config.BindAddress),
			zap.Error(err))
		return err
	}

	s.conn, err = net.ListenUDP("udp", s.addr)
	if err != nil {
		s.Logger.Info("Failed to set up UDP listener",
			zap.Stringer("addr", s.addr),
			zap.Error(err))
		return err
	}

	if s.config.ReadBuffer != 0 {
		err = s.conn.SetReadBuffer(s.config.ReadBuffer)
		if err != nil {
			s.Logger.Info("Failed to set UDP read buffer",
				zap.Int("read_buffer", s.config.ReadBuffer),
				zap.Error(err))
			return err
		}
	}

	s.Logger.Info("Started listening on UDP", zap.String("addr", s.config.BindAddress))

	s.wg.Add(3)
	go s.serve()
	go s.parser()
	go s.writer()

	return nil
}

func (s *Service) writer() {
	defer s.wg.Done()

	for {
		select {
		case batch := <-s.batcher.Out():
			if err := s.PointsWriter.WritePoints(&coordinator.WritePointsRequest{
				Database:         s.config.Database,
				RetentionPolicy:  s.config.RetentionPolicy,
				ConsistencyLevel: coordinator.ConsistencyLevelOne,
				Points:           batch,
			}); err == nil {
				s.statMap.Add(statBatchesTrasmitted, 1)
				s.statMap.Add(statPointsTransmitted, int64(len(batch)))
			} else {
				s.Logger.Info("failed to write point batch to database",
					logger.Database(s.config.Database),
					zap.Error(err))
				s.statMap.Add(statBatchesTransmitFail, 1)
			}

		case <-s.done:
			return
		}
	}
}

func (s *Service) serve() {
	defer s.wg.Done()

	s.batcher.Start()
	for {

		select {
		case <-s.done:
			// We closed the connection, time to go.
			return
		default:
			// Keep processing.
			buf := make([]byte, s.config.UDPPayloadSize)
			n, _, err := s.conn.ReadFromUDP(buf)
			if err != nil {
				s.statMap.Add(statReadFail, 1)
				s.Logger.Info("Failed to read UDP message", zap.Error(err))
				continue
			}
			s.statMap.Add(statBytesReceived, int64(n))
			s.parserChan <- buf[:n]
		}
	}
}

func (s *Service) parser() {
	defer s.wg.Done()

	for {
		select {
		case <-s.done:
			return
		case buf := <-s.parserChan:
			points, err := models.ParsePointsWithPrecision(buf, time.Now().UTC(), s.config.Precision)
			if err != nil {
				s.statMap.Add(statPointsParseFail, 1)
				s.Logger.Info("Failed to parse points", zap.Error(err))
				continue
			}

			for _, point := range points {
				s.batcher.In() <- point
			}
			s.statMap.Add(statPointsReceived, int64(len(points)))
		}
	}
}

// Close closes the underlying listener.
func (s *Service) Close() error {
	if s.conn == nil {
		return errors.New("Service already closed")
	}

	s.conn.Close()
	s.batcher.Flush()
	close(s.done)
	s.wg.Wait()

	// Release all remaining resources.
	s.done = nil
	s.conn = nil

	s.Logger.Info("Service closed")

	return nil
}

// WithLogger sets the logger on the service.
func (s *Service) WithLogger(log *zap.Logger) {
	s.Logger = log.With(zap.String("service", "udp"))
}

// Addr returns the listener's address
func (s *Service) Addr() net.Addr {
	return s.addr
}
