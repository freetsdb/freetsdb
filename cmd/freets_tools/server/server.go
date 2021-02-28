package server

import (
	"time"

	"github.com/freetsdb/freetsdb/services/meta"
	"github.com/freetsdb/freetsdb/tsdb"
	"go.uber.org/zap"
)

type Interface interface {
	Open(path string) error
	Close()
	MetaClient() MetaClient
	TSDBConfig() tsdb.Config
	Logger() *zap.Logger
}

type MetaClient interface {
	Database(name string) *meta.DatabaseInfo
	RetentionPolicy(database, name string) (rpi *meta.RetentionPolicyInfo, err error)
	ShardGroupsByTimeRange(database, policy string, min, max time.Time) (a []meta.ShardGroupInfo, err error)
	CreateRetentionPolicy(database string, rpi *meta.RetentionPolicyInfo) (*meta.RetentionPolicyInfo, error)
	UpdateRetentionPolicy(database, name string, rpu *meta.RetentionPolicyUpdate) error
	CreateDatabase(name string) (*meta.DatabaseInfo, error)
	CreateDatabaseWithRetentionPolicy(name string, rpi *meta.RetentionPolicyInfo) (*meta.DatabaseInfo, error)
	DeleteShardGroup(database, policy string, id uint64) error
	CreateShardGroup(database, policy string, timestamp time.Time) (*meta.ShardGroupInfo, error)
}
