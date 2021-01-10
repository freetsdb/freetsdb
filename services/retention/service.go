package retention // import "github.com/freetsdb/freetsdb/services/retention"

import (
	"sync"
	"time"

	"github.com/freetsdb/freetsdb/logger"
	"github.com/freetsdb/freetsdb/services/meta"
	"go.uber.org/zap"
)

// Service represents the retention policy enforcement service.
type Service struct {
	MetaClient interface {
		Databases() ([]meta.DatabaseInfo, error)
		DeleteShardGroup(database, policy string, id uint64) error
	}
	TSDBStore interface {
		ShardIDs() []uint64
		DeleteShard(shardID uint64) error
	}

	enabled       bool
	checkInterval time.Duration
	wg            sync.WaitGroup
	done          chan struct{}

	logger *zap.Logger
}

// NewService returns a configured retention policy enforcement service.
func NewService(c Config) *Service {
	return &Service{
		checkInterval: time.Duration(c.CheckInterval),
		done:          make(chan struct{}),
		logger:        zap.NewNop(),
	}
}

// Open starts retention policy enforcement.
func (s *Service) Open() error {
	s.logger.Info("Starting retention policy enforcement service",
		logger.DurationLiteral("check_interval", time.Duration(s.checkInterval)))

	s.wg.Add(2)
	go s.deleteShardGroups()
	go s.deleteShards()
	return nil
}

// Close stops retention policy enforcement.
func (s *Service) Close() error {
	s.logger.Info("Retention policy enforcement terminating")
	close(s.done)
	s.wg.Wait()
	return nil
}

// WithLogger sets the logger on the service.
func (s *Service) WithLogger(log *zap.Logger) {
	s.logger = log.With(zap.String("service", "retention"))
}

func (s *Service) deleteShardGroups() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return

		case <-ticker.C:
			dbs, err := s.MetaClient.Databases()
			if err != nil {
				s.logger.Info("error getting databases", zap.Error(err))
				continue
			}

			for _, d := range dbs {
				for _, r := range d.RetentionPolicies {
					for _, g := range r.ExpiredShardGroups(time.Now().UTC()) {
						if err := s.MetaClient.DeleteShardGroup(d.Name, r.Name, g.ID); err != nil {
							s.logger.Info("Failed to delete shard group",
								logger.Database(d.Name),
								logger.ShardGroup(g.ID),
								logger.RetentionPolicy(r.Name),
								zap.Error(err))
						} else {
							s.logger.Info("Deleted shard group",
								logger.Database(d.Name),
								logger.ShardGroup(g.ID),
								logger.RetentionPolicy(r.Name))
						}
					}
				}
			}
		}
	}
}

func (s *Service) deleteShards() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return

		case <-ticker.C:
			s.logger.Info("Retention policy shard deletion check commencing")

			type deletionInfo struct {
				db string
				rp string
			}
			deletedShardIDs := make(map[uint64]deletionInfo, 0)
			dbs, err := s.MetaClient.Databases()
			if err != nil {
				s.logger.Info("Error getting databases", zap.Error(err))
			}
			for _, d := range dbs {
				for _, r := range d.RetentionPolicies {
					for _, g := range r.DeletedShardGroups() {
						for _, sh := range g.Shards {
							deletedShardIDs[sh.ID] = deletionInfo{db: d.Name, rp: r.Name}
						}
					}
				}
			}

			for _, id := range s.TSDBStore.ShardIDs() {
				if di, ok := deletedShardIDs[id]; ok {
					if err := s.TSDBStore.DeleteShard(id); err != nil {
						s.logger.Info("Failed to delete shard",
							logger.Shard(id),
							logger.Database(di.db),
							logger.RetentionPolicy(di.rp),
							zap.Error(err))
						continue
					}
					s.logger.Info("Deleted shard",
						logger.Shard(id),
						logger.Database(di.db),
						logger.RetentionPolicy(di.rp))
				}
			}
		}
	}
}
