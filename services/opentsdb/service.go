package opentsdb // import "github.com/freetsdb/freetsdb/services/opentsdb"

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"expvar"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
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

const leaderWaitTimeout = 30 * time.Second

// statistics gathered by the openTSDB package.
const (
	statHTTPConnectionsHandled   = "httpConnsHandled"
	statTelnetConnectionsActive  = "tlConnsActive"
	statTelnetConnectionsHandled = "tlConnsHandled"
	statTelnetPointsReceived     = "tlPointsRx"
	statTelnetBytesReceived      = "tlBytesRx"
	statTelnetReadError          = "tlReadErr"
	statTelnetBadLine            = "tlBadLine"
	statTelnetBadTime            = "tlBadTime"
	statTelnetBadTag             = "tlBadTag"
	statTelnetBadFloat           = "tlBadFloat"
	statBatchesTrasmitted        = "batchesTx"
	statPointsTransmitted        = "pointsTx"
	statBatchesTransmitFail      = "batchesTxFail"
	statConnectionsActive        = "connsActive"
	statConnectionsHandled       = "connsHandled"
	statDroppedPointsInvalid     = "droppedPointsInvalid"
)

// Service manages the listener and handler for an HTTP endpoint.
type Service struct {
	ln     net.Listener  // main listener
	httpln *chanListener // http channel-based listener

	mu   sync.Mutex
	wg   sync.WaitGroup
	done chan struct{}
	err  chan error
	tls  bool
	cert string

	BindAddress      string
	Database         string
	RetentionPolicy  string
	ConsistencyLevel coordinator.ConsistencyLevel

	PointsWriter interface {
		WritePoints(p *coordinator.WritePointsRequest) error
	}
	MetaClient interface {
		CreateDatabase(name string) (*meta.DatabaseInfo, error)
	}

	// Points received over the telnet protocol are batched.
	batchSize    int
	batchPending int
	batchTimeout time.Duration
	batcher      *tsdb.PointBatcher

	LogPointErrors bool
	Logger         *zap.Logger
	statMap        *expvar.Map
}

// NewService returns a new instance of Service.
func NewService(c Config) (*Service, error) {
	consistencyLevel, err := coordinator.ParseConsistencyLevel(c.ConsistencyLevel)
	if err != nil {
		return nil, err
	}

	s := &Service{
		done:             make(chan struct{}),
		tls:              c.TLSEnabled,
		cert:             c.Certificate,
		err:              make(chan error),
		BindAddress:      c.BindAddress,
		Database:         c.Database,
		RetentionPolicy:  c.RetentionPolicy,
		ConsistencyLevel: consistencyLevel,
		batchSize:        c.BatchSize,
		batchPending:     c.BatchPending,
		batchTimeout:     time.Duration(c.BatchTimeout),
		Logger:           zap.NewNop(),
		LogPointErrors:   c.LogPointErrors,
	}
	return s, nil
}

// Open starts the service
func (s *Service) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Logger.Info("Starting OpenTSDB service")

	// Configure expvar monitoring. It's OK to do this even if the service fails to open and
	// should be done before any data could arrive for the service.
	key := strings.Join([]string{"opentsdb", s.BindAddress}, ":")
	tags := map[string]string{"bind": s.BindAddress}
	s.statMap = freetsdb.NewStatistics(key, "opentsdb", tags)

	if _, err := s.MetaClient.CreateDatabase(s.Database); err != nil {
		s.Logger.Info("Failed to ensure target database", logger.Database(s.Database), zap.Error(err))
		return err
	}

	s.batcher = tsdb.NewPointBatcher(s.batchSize, s.batchPending, s.batchTimeout)
	s.batcher.Start()

	// Start processing batches.
	s.wg.Add(1)
	go s.processBatches(s.batcher)

	// Open listener.
	if s.tls {
		cert, err := tls.LoadX509KeyPair(s.cert, s.cert)
		if err != nil {
			return err
		}

		listener, err := tls.Listen("tcp", s.BindAddress, &tls.Config{
			Certificates: []tls.Certificate{cert},
		})
		if err != nil {
			return err
		}

		s.Logger.Info("Listening on TLS", zap.Stringer("addr", listener.Addr()))
		s.ln = listener
	} else {
		listener, err := net.Listen("tcp", s.BindAddress)
		if err != nil {
			return err
		}

		s.Logger.Info("Listening on", zap.Stringer("addr", listener.Addr()))
		s.ln = listener
	}
	s.httpln = newChanListener(s.ln.Addr())

	// Begin listening for connections.
	s.wg.Add(2)
	go s.serveHTTP()
	go s.serve()

	return nil
}

// Close closes the openTSDB service
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ln != nil {
		return s.ln.Close()
	}

	if s.batcher != nil {
		s.batcher.Stop()
	}
	close(s.done)
	s.wg.Wait()
	return nil
}

// WithLogger sets the logger on the service.
func (s *Service) WithLogger(log *zap.Logger) {
	s.Logger = log.With(zap.String("service", "opentsdb"))
}

// Err returns a channel for fatal errors that occur on the listener.
func (s *Service) Err() <-chan error { return s.err }

// Addr returns the listener's address. Returns nil if listener is closed.
func (s *Service) Addr() net.Addr {
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// serve serves the handler from the listener.
func (s *Service) serve() {
	defer s.wg.Done()

	for {
		// Wait for next connection.
		conn, err := s.ln.Accept()
		if opErr, ok := err.(*net.OpError); ok && !opErr.Temporary() {
			s.Logger.Info("OpenTSDB TCP listener closed")
			return
		} else if err != nil {
			s.Logger.Info("Error accepting openTSDB", zap.Error(err))
			continue
		}

		// Handle connection in separate goroutine.
		go s.handleConn(conn)
	}
}

// handleConn processes conn. This is run in a separate goroutine.
func (s *Service) handleConn(conn net.Conn) {
	defer s.statMap.Add(statConnectionsActive, -1)
	s.statMap.Add(statConnectionsActive, 1)
	s.statMap.Add(statConnectionsHandled, 1)

	// Read header into buffer to check if it's HTTP.
	var buf bytes.Buffer
	r := bufio.NewReader(io.TeeReader(conn, &buf))

	// Attempt to parse connection as HTTP.
	_, err := http.ReadRequest(r)

	// Rebuild connection from buffer and remaining connection data.
	bufr := bufio.NewReader(io.MultiReader(&buf, conn))
	conn = &readerConn{Conn: conn, r: bufr}

	// If no HTTP parsing error occurred then process as HTTP.
	if err == nil {
		s.statMap.Add(statHTTPConnectionsHandled, 1)
		s.httpln.ch <- conn
		return
	}

	// Otherwise handle in telnet format.
	s.wg.Add(1)
	s.handleTelnetConn(conn)
}

// handleTelnetConn accepts OpenTSDB's telnet protocol.
// Each telnet command consists of a line of the form:
//   put sys.cpu.user 1356998400 42.5 host=webserver01 cpu=0
func (s *Service) handleTelnetConn(conn net.Conn) {
	defer conn.Close()
	defer s.wg.Done()
	defer s.statMap.Add(statTelnetConnectionsActive, -1)
	s.statMap.Add(statTelnetConnectionsActive, 1)
	s.statMap.Add(statTelnetConnectionsHandled, 1)

	// Get connection details.
	remoteAddr := conn.RemoteAddr().String()

	// Wrap connection in a text protocol reader.
	r := textproto.NewReader(bufio.NewReader(conn))
	for {
		line, err := r.ReadLine()
		if err != nil {
			if err != io.EOF {
				s.statMap.Add(statTelnetReadError, 1)
				s.Logger.Info("Error reading from openTSDB connection", zap.Error(err))
			}
			return
		}
		s.statMap.Add(statTelnetPointsReceived, 1)
		s.statMap.Add(statTelnetBytesReceived, int64(len(line)))

		inputStrs := strings.Fields(line)

		if len(inputStrs) == 1 && inputStrs[0] == "version" {
			conn.Write([]byte("FreeTSDB TSDB proxy"))
			continue
		}

		if len(inputStrs) < 4 || inputStrs[0] != "put" {
			s.statMap.Add(statTelnetBadLine, 1)
			if s.LogPointErrors {
				s.Logger.Info("malformed line", zap.String("line", line), zap.String("remote_addr", remoteAddr))
			}
			continue
		}

		measurement := inputStrs[1]
		tsStr := inputStrs[2]
		valueStr := inputStrs[3]
		tagStrs := inputStrs[4:]

		var t time.Time
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			s.statMap.Add(statTelnetBadTime, 1)
			if s.LogPointErrors {
				s.Logger.Info("malformed time", zap.String("time", tsStr), zap.String("remote_addr", remoteAddr))
			}
		}

		switch len(tsStr) {
		case 10:
			t = time.Unix(ts, 0)
			break
		case 13:
			t = time.Unix(ts/1000, (ts%1000)*1000)
			break
		default:
			s.statMap.Add(statTelnetBadTime, 1)
			if s.LogPointErrors {
				s.Logger.Info("Time must be 10 or 13 chars",
					zap.String("time", tsStr),
					zap.String("remote_addr", remoteAddr))
			}
			continue
		}

		tags := make(map[string]string)
		for t := range tagStrs {
			parts := strings.SplitN(tagStrs[t], "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				s.statMap.Add(statTelnetBadTag, 1)
				if s.LogPointErrors {
					s.Logger.Info("Malformed tag data",
						zap.String("tag", tagStrs[t]),
						zap.String("remote_addr", remoteAddr))
				}
				continue
			}
			k := parts[0]

			tags[k] = parts[1]
		}

		fields := make(map[string]interface{})
		fv, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			s.statMap.Add(statTelnetBadFloat, 1)
			if s.LogPointErrors {
				s.Logger.Info("Bad float", zap.String("value", valueStr), zap.String("remote_addr", remoteAddr))
			}
			continue
		}
		fields["value"] = fv

		pt, err := models.NewPoint(measurement, tags, fields, t)
		if err != nil {
			s.statMap.Add(statTelnetBadFloat, 1)
			if s.LogPointErrors {
				s.Logger.Info("Bad float", zap.String("value", valueStr), zap.String("remote_addr", remoteAddr))
			}
			continue
		}
		s.batcher.In() <- pt
	}
}

// serveHTTP handles connections in HTTP format.
func (s *Service) serveHTTP() {
	srv := &http.Server{Handler: &Handler{
		Database:         s.Database,
		RetentionPolicy:  s.RetentionPolicy,
		ConsistencyLevel: s.ConsistencyLevel,
		PointsWriter:     s.PointsWriter,
		Logger:           s.Logger,
		statMap:          s.statMap,
	}}
	srv.Serve(s.httpln)
}

// processBatches continually drains the given batcher and writes the batches to the database.
func (s *Service) processBatches(batcher *tsdb.PointBatcher) {
	defer s.wg.Done()
	for {
		select {
		case batch := <-batcher.Out():
			if err := s.PointsWriter.WritePoints(&coordinator.WritePointsRequest{
				Database:         s.Database,
				RetentionPolicy:  s.RetentionPolicy,
				ConsistencyLevel: s.ConsistencyLevel,
				Points:           batch,
			}); err == nil {
				s.statMap.Add(statBatchesTrasmitted, 1)
				s.statMap.Add(statPointsTransmitted, int64(len(batch)))
			} else {
				s.Logger.Info("Required database does not yet exist", logger.Database(s.Database), zap.Error(err))
				s.statMap.Add(statBatchesTransmitFail, 1)
			}

		case <-s.done:
			return
		}
	}
}
