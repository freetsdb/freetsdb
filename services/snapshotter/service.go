package snapshotter // import "github.com/freetsdb/freetsdb/services/snapshotter"

import (
	"bytes"
	"encoding"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/freetsdb/freetsdb"
	"github.com/freetsdb/freetsdb/services/meta"
	"github.com/freetsdb/freetsdb/tsdb"
	"go.uber.org/zap"
)

const (
	// MuxHeader is the header byte used for the TCP muxer.
	MuxHeader = 3

	// BackupMagicHeader is the first 8 bytes used to identify and validate
	// a metastore backup file
	BackupMagicHeader = 0x59590101
)

// Service manages the listener for the snapshot endpoint.
type Service struct {
	wg  sync.WaitGroup
	err chan error

	Node *freetsdb.Node

	MetaClient interface {
		encoding.BinaryMarshaler
		Database(name string) (*meta.DatabaseInfo, error)
	}

	TSDBStore *tsdb.Store

	Listener net.Listener
	Logger   *zap.Logger
}

// NewService returns a new instance of Service.
func NewService() *Service {
	return &Service{
		err:    make(chan error),
		Logger: zap.NewNop(),
	}
}

// Open starts the service.
func (s *Service) Open() error {
	s.Logger.Info("Starting snapshot service")

	s.wg.Add(1)
	go s.serve()
	return nil
}

// Close implements the Service interface.
func (s *Service) Close() error {
	if s.Listener != nil {
		s.Listener.Close()
	}
	s.wg.Wait()
	return nil
}

// WithLogger sets the logger on the service.
func (s *Service) WithLogger(log *zap.Logger) {
	s.Logger = log.With(zap.String("service", "snapshot"))
}

// Err returns a channel for fatal out-of-band errors.
func (s *Service) Err() <-chan error { return s.err }

// serve serves snapshot requests from the listener.
func (s *Service) serve() {
	defer s.wg.Done()

	for {
		// Wait for next connection.
		conn, err := s.Listener.Accept()
		if err != nil && strings.Contains(err.Error(), "connection closed") {
			s.Logger.Info("Snapshot listener closed")
			return
		} else if err != nil {
			s.Logger.Info("Error accepting snapshot request", zap.Error(err))
			continue
		}

		// Handle connection in separate goroutine.
		s.wg.Add(1)
		go func(conn net.Conn) {
			defer s.wg.Done()
			defer conn.Close()
			if err := s.handleConn(conn); err != nil {
				s.Logger.Info(err.Error())
			}
		}(conn)
	}
}

// handleConn processes conn. This is run in a separate goroutine.
func (s *Service) handleConn(conn net.Conn) error {
	r, err := s.readRequest(conn)
	if err != nil {
		return fmt.Errorf("read request: %s", err)
	}

	switch r.Type {
	case RequestShardBackup:
		if err := s.TSDBStore.BackupShard(r.ShardID, r.Since, conn); err != nil {
			return err
		}
	case RequestMetastoreBackup:
		if err := s.writeMetaStore(conn); err != nil {
			return err
		}
	case RequestDatabaseInfo:
		return s.writeDatabaseInfo(conn, r.Database)
	case RequestRetentionPolicyInfo:
		return s.writeRetentionPolicyInfo(conn, r.Database, r.RetentionPolicy)
	default:
		return fmt.Errorf("request type unknown: %v", r.Type)
	}

	return nil
}

func (s *Service) writeMetaStore(conn net.Conn) error {
	// Retrieve and serialize the current meta data.
	metaBlob, err := s.MetaClient.MarshalBinary()
	if err != nil {
		return fmt.Errorf("marshal meta: %s", err)
	}

	var nodeBytes bytes.Buffer
	if err := json.NewEncoder(&nodeBytes).Encode(s.Node); err != nil {
		return err
	}

	var numBytes [24]byte

	binary.BigEndian.PutUint64(numBytes[:8], BackupMagicHeader)
	binary.BigEndian.PutUint64(numBytes[8:16], uint64(len(metaBlob)))
	binary.BigEndian.PutUint64(numBytes[16:24], uint64(nodeBytes.Len()))

	// backup header followed by meta blob length
	if _, err := conn.Write(numBytes[:16]); err != nil {
		return err
	}

	if _, err := conn.Write(metaBlob); err != nil {
		return err
	}

	if _, err := conn.Write(numBytes[16:24]); err != nil {
		return err
	}

	if _, err := nodeBytes.WriteTo(conn); err != nil {
		return err
	}
	return nil
}

// writeDatabaseInfo will write the relative paths of all shards in the database on
// this server into the connection
func (s *Service) writeDatabaseInfo(conn net.Conn, database string) error {
	res := Response{}
	db, err := s.MetaClient.Database(database)
	if err != nil {
		return err
	}
	if db == nil {
		return freetsdb.ErrDatabaseNotFound(database)
	}

	for _, rp := range db.RetentionPolicies {
		for _, sg := range rp.ShardGroups {
			for _, sh := range sg.Shards {
				// ignore if the shard isn't on the server
				if s.TSDBStore.Shard(sh.ID) == nil {
					continue
				}

				path, err := s.TSDBStore.ShardRelativePath(sh.ID)
				if err != nil {
					return err
				}

				res.Paths = append(res.Paths, path)
			}
		}
	}

	if err := json.NewEncoder(conn).Encode(res); err != nil {
		return fmt.Errorf("encode resonse: %s", err.Error())
	}

	return nil
}

// writeDatabaseInfo will write the relative paths of all shards in the retention policy on
// this server into the connection
func (s *Service) writeRetentionPolicyInfo(conn net.Conn, database, retentionPolicy string) error {
	res := Response{}
	db, err := s.MetaClient.Database(database)
	if err != nil {
		return err
	}
	if db == nil {
		return freetsdb.ErrDatabaseNotFound(database)
	}

	var ret *meta.RetentionPolicyInfo

	for _, rp := range db.RetentionPolicies {
		if rp.Name == retentionPolicy {
			ret = &rp
			break
		}
	}

	if ret == nil {
		return freetsdb.ErrRetentionPolicyNotFound(retentionPolicy)
	}

	for _, sg := range ret.ShardGroups {
		for _, sh := range sg.Shards {
			// ignore if the shard isn't on the server
			if s.TSDBStore.Shard(sh.ID) == nil {
				continue
			}

			path, err := s.TSDBStore.ShardRelativePath(sh.ID)
			if err != nil {
				return err
			}

			res.Paths = append(res.Paths, path)
		}
	}

	if err := json.NewEncoder(conn).Encode(res); err != nil {
		return fmt.Errorf("encode resonse: %s", err.Error())
	}

	return nil
}

// readRequest Unmarshals a request object from the conn
func (s *Service) readRequest(conn net.Conn) (Request, error) {
	var r Request
	if err := json.NewDecoder(conn).Decode(&r); err != nil {
		return r, err
	}
	return r, nil
}

type RequestType uint8

const (
	RequestShardBackup RequestType = iota
	RequestMetastoreBackup
	RequestDatabaseInfo
	RequestRetentionPolicyInfo
)

// Request represents a request for a specific backup or for information
// about the shards on this server for a database or retention policy
type Request struct {
	Type            RequestType
	Database        string
	RetentionPolicy string
	ShardID         uint64
	Since           time.Time
}

// Response contains the relative paths for all the shards on this server
// that are in the requested database or retention policy
type Response struct {
	Paths []string
}
