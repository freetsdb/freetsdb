package monitor // import "github.com/freetsdb/freetsdb/monitor"

import (
	"expvar"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/freetsdb/freetsdb"
	"github.com/freetsdb/freetsdb/logger"
	"github.com/freetsdb/freetsdb/models"
	"github.com/freetsdb/freetsdb/monitor/diagnostics"
	"github.com/freetsdb/freetsdb/services/meta"
	"go.uber.org/zap"
)

const leaderWaitTimeout = 30 * time.Second

// Policy constants.
const (
	MonitorRetentionPolicy         = "monitor"
	MonitorRetentionPolicyDuration = 7 * 24 * time.Hour
)

// Monitor represents an instance of the monitor system.
type Monitor struct {
	// Build information for diagnostics.
	Version   string
	Commit    string
	Branch    string
	BuildTime string

	wg   sync.WaitGroup
	done chan struct{}
	mu   sync.Mutex

	diagRegistrations map[string]diagnostics.Client

	storeCreated           bool
	storeEnabled           bool
	storeDatabase          string
	storeRetentionPolicy   string
	storeRetentionDuration time.Duration
	storeReplicationFactor int
	storeAddress           string
	storeInterval          time.Duration

	MetaClient interface {
		ClusterID() uint64
		CreateDatabase(name string) (*meta.DatabaseInfo, error)
		CreateRetentionPolicy(database string, rpi *meta.RetentionPolicyInfo) (*meta.RetentionPolicyInfo, error)
		SetDefaultRetentionPolicy(database, name string) error
		DropRetentionPolicy(database, name string) error
	}

	NodeID uint64

	// Writer for pushing stats back into the database.
	// This causes a circular dependency if it depends on cluster directly so it
	// is wrapped in a simpler interface.
	PointsWriter interface {
		WritePoints(database, retentionPolicy string, points models.Points) error
	}

	Logger *zap.Logger
}

// New returns a new instance of the monitor system.
func New(c Config) *Monitor {
	return &Monitor{
		done:                 make(chan struct{}),
		diagRegistrations:    make(map[string]diagnostics.Client),
		storeEnabled:         c.StoreEnabled,
		storeDatabase:        c.StoreDatabase,
		storeInterval:        time.Duration(c.StoreInterval),
		storeRetentionPolicy: MonitorRetentionPolicy,
		Logger:               zap.NewNop(),
	}
}

// Open opens the monitoring system, using the given clusterID, node ID, and hostname
// for identification purpose.
func (m *Monitor) Open() error {
	m.Logger.Info("Starting monitor system")

	// Self-register various stats and diagnostics.
	m.RegisterDiagnosticsClient("build", &build{
		Version: m.Version,
		Commit:  m.Commit,
		Branch:  m.Branch,
		Time:    m.BuildTime,
	})
	m.RegisterDiagnosticsClient("runtime", &goRuntime{})
	m.RegisterDiagnosticsClient("network", &network{})
	m.RegisterDiagnosticsClient("system", &system{})

	// If enabled, record stats in a FreeTSDB system.
	if m.storeEnabled {

		// Start periodic writes to system.
		m.wg.Add(1)
		go m.storeStatistics()
	}

	return nil
}

// Close closes the monitor system.
func (m *Monitor) Close() {
	m.Logger.Info("Shutting down monitor system")
	close(m.done)
	m.wg.Wait()
	m.done = nil
}

// WithLogger sets the logger for the Monitor.
func (m *Monitor) WithLogger(log *zap.Logger) {
	m.Logger = log.With(zap.String("service", "monitor"))
}

// RegisterDiagnosticsClient registers a diagnostics client with the given name and tags.
func (m *Monitor) RegisterDiagnosticsClient(name string, client diagnostics.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diagRegistrations[name] = client
	m.Logger.Info("Registered for diagnostics monitoring", zap.String("name", name))
}

// DeregisterDiagnosticsClient deregisters a diagnostics client by name.
func (m *Monitor) DeregisterDiagnosticsClient(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.diagRegistrations, name)
}

// Statistics returns the combined statistics for all expvar data. The given
// tags are added to each of the returned statistics.
func (m *Monitor) Statistics(tags map[string]string) ([]*Statistic, error) {
	var statistics []*Statistic

	expvar.Do(func(kv expvar.KeyValue) {
		// Skip built-in expvar stats.
		if kv.Key == "memstats" || kv.Key == "cmdline" {
			return
		}

		statistic := &Statistic{
			Tags:   make(map[string]string),
			Values: make(map[string]interface{}),
		}

		// Add any supplied tags.
		for k, v := range tags {
			statistic.Tags[k] = v
		}

		// Every other top-level expvar value is a map.
		m := kv.Value.(*expvar.Map)

		m.Do(func(subKV expvar.KeyValue) {
			switch subKV.Key {
			case "name":
				// straight to string name.
				u, err := strconv.Unquote(subKV.Value.String())
				if err != nil {
					return
				}
				statistic.Name = u
			case "tags":
				// string-string tags map.
				n := subKV.Value.(*expvar.Map)
				n.Do(func(t expvar.KeyValue) {
					u, err := strconv.Unquote(t.Value.String())
					if err != nil {
						return
					}
					statistic.Tags[t.Key] = u
				})
			case "values":
				// string-interface map.
				n := subKV.Value.(*expvar.Map)
				n.Do(func(kv expvar.KeyValue) {
					var f interface{}
					var err error
					switch v := kv.Value.(type) {
					case *expvar.Float:
						f, err = strconv.ParseFloat(v.String(), 64)
						if err != nil {
							return
						}
					case *expvar.Int:
						f, err = strconv.ParseInt(v.String(), 10, 64)
						if err != nil {
							return
						}
					default:
						return
					}
					statistic.Values[kv.Key] = f
				})
			}
		})

		// If a registered client has no field data, don't include it in the results
		if len(statistic.Values) == 0 {
			return
		}

		statistics = append(statistics, statistic)
	})

	// Add Go memstats.
	statistic := &Statistic{
		Name:   "runtime",
		Tags:   make(map[string]string),
		Values: make(map[string]interface{}),
	}

	// Add any supplied tags to Go memstats
	for k, v := range tags {
		statistic.Tags[k] = v
	}

	var rt runtime.MemStats
	runtime.ReadMemStats(&rt)
	statistic.Values = map[string]interface{}{
		"Alloc":        int64(rt.Alloc),
		"TotalAlloc":   int64(rt.TotalAlloc),
		"Sys":          int64(rt.Sys),
		"Lookups":      int64(rt.Lookups),
		"Mallocs":      int64(rt.Mallocs),
		"Frees":        int64(rt.Frees),
		"HeapAlloc":    int64(rt.HeapAlloc),
		"HeapSys":      int64(rt.HeapSys),
		"HeapIdle":     int64(rt.HeapIdle),
		"HeapInUse":    int64(rt.HeapInuse),
		"HeapReleased": int64(rt.HeapReleased),
		"HeapObjects":  int64(rt.HeapObjects),
		"PauseTotalNs": int64(rt.PauseTotalNs),
		"NumGC":        int64(rt.NumGC),
		"NumGoroutine": int64(runtime.NumGoroutine()),
	}
	statistics = append(statistics, statistic)

	return statistics, nil
}

// Diagnostics fetches diagnostic information for each registered
// diagnostic client. It skips any clients that return an error when
// retrieving their diagnostics.
func (m *Monitor) Diagnostics() (map[string]*diagnostics.Diagnostics, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	diags := make(map[string]*diagnostics.Diagnostics, len(m.diagRegistrations))
	for k, v := range m.diagRegistrations {
		d, err := v.Diagnostics()
		if err != nil {
			continue
		}
		diags[k] = d
	}
	return diags, nil
}

// createInternalStorage ensures the internal storage has been created.
func (m *Monitor) createInternalStorage() {
	if m.storeCreated {
		return
	}

	if _, err := m.MetaClient.CreateDatabase(m.storeDatabase); err != nil {
		m.Logger.Info("Failed to create database",
			logger.Database(m.storeDatabase),
			zap.Error(err))
		return
	}

	rpi := meta.NewRetentionPolicyInfo(MonitorRetentionPolicy)
	rpi.Duration = MonitorRetentionPolicyDuration
	rpi.ReplicaN = 1
	if _, err := m.MetaClient.CreateRetentionPolicy(m.storeDatabase, rpi); err != nil {
		m.Logger.Info("Failed to create retention policy",
			logger.RetentionPolicy(rpi.Name),
			zap.Error(err))
		return
	}

	if err := m.MetaClient.SetDefaultRetentionPolicy(m.storeDatabase, rpi.Name); err != nil {
		m.Logger.Info("Failed to set default retention policy",
			logger.Database(m.storeDatabase),
			zap.Error(err))
		return
	}

	err := m.MetaClient.DropRetentionPolicy(m.storeDatabase, "default")
	if err != nil && err.Error() != freetsdb.ErrRetentionPolicyNotFound("default").Error() {
		m.Logger.Info("Failed to delete retention policy 'default'",
			logger.Database(m.storeDatabase),
			zap.Error(err))
		return
	}

	// Mark storage creation complete.
	m.storeCreated = true
}

// storeStatistics writes the statistics to an FreeTSDB system.
func (m *Monitor) storeStatistics() {
	defer m.wg.Done()
	m.Logger.Info("Storing statistics",
		logger.Database(m.storeDatabase),
		logger.RetentionPolicy(m.storeRetentionPolicy),
		logger.DurationLiteral("interval", m.storeInterval))

	// Get cluster-level metadata. Nothing different is going to happen if errors occur.
	clusterID := m.MetaClient.ClusterID()
	hostname, _ := os.Hostname()
	clusterTags := map[string]string{
		"clusterID": fmt.Sprintf("%d", clusterID),
		"nodeID":    fmt.Sprintf("%d", m.NodeID),
		"hostname":  hostname,
	}

	tick := time.NewTicker(m.storeInterval)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			m.createInternalStorage()

			stats, err := m.Statistics(clusterTags)
			if err != nil {
				m.Logger.Info("Failed to retrieve registered statistics",
					zap.Error(err))
				continue
			}

			points := make(models.Points, 0, len(stats))
			for _, s := range stats {
				pt, err := models.NewPoint(s.Name, s.Tags, s.Values, time.Now().Truncate(time.Second))
				if err != nil {
					m.Logger.Info("Dropping point",
						zap.String("name", s.Name),
						zap.Error(err))
					continue
				}
				points = append(points, pt)
			}

			if err := m.PointsWriter.WritePoints(m.storeDatabase, m.storeRetentionPolicy, points); err != nil {
				m.Logger.Info("Failed to store statistics", zap.Error(err))
			}
		case <-m.done:
			m.Logger.Info("terminating storage of statistics")
			return
		}

	}
}

// Statistic represents the information returned by a single monitor client.
type Statistic struct {
	Name   string                 `json:"name"`
	Tags   map[string]string      `json:"tags"`
	Values map[string]interface{} `json:"values"`
}

// newStatistic returns a new statistic object.
func newStatistic(name string, tags map[string]string, values map[string]interface{}) *Statistic {
	return &Statistic{
		Name:   name,
		Tags:   tags,
		Values: values,
	}
}

// valueNames returns a sorted list of the value names, if any.
func (s *Statistic) ValueNames() []string {
	a := make([]string, 0, len(s.Values))
	for k := range s.Values {
		a = append(a, k)
	}
	sort.Strings(a)
	return a
}

// DiagnosticsFromMap returns a Diagnostics from a map.
func DiagnosticsFromMap(m map[string]interface{}) *diagnostics.Diagnostics {
	// Display columns in deterministic order.
	sortedKeys := make([]string, 0, len(m))
	for k := range m {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)

	d := diagnostics.NewDiagnostics(sortedKeys)
	row := make([]interface{}, len(sortedKeys))
	for i, k := range sortedKeys {
		row[i] = m[k]
	}
	d.AddRow(row)

	return d
}
