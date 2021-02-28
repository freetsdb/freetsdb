// Package meta provides control over meta data for FreeTSDB,
// such as controlling databases, retention policies, users, etc.
package meta

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/freetsdb/freetsdb"
	"github.com/freetsdb/freetsdb/pkg/file"
	"github.com/freetsdb/freetsdb/services/influxql"
	"github.com/freetsdb/freetsdb/services/meta/internal"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

const (
	// errSleep is the time to sleep after we've failed on every metaserver
	// before making another pass
	errSleep = time.Second

	// maxRetries is the maximum number of attemps to make before returning
	// a failure to the caller
	maxRetries = 10

	// SaltBytes is the number of bytes used for salts.
	SaltBytes = 32

	metaFile = "meta.db"

	// ShardGroupDeletedExpiration is the amount of time before a shard group info will be removed from cached
	// data after it has been marked deleted (2 weeks).
	ShardGroupDeletedExpiration = -2 * 7 * 24 * time.Hour
)

var (
	// ErrServiceUnavailable is returned when the meta service is unavailable.
	ErrServiceUnavailable = errors.New("meta service unavailable")

	// ErrService is returned when the meta service returns an error.
	ErrService = errors.New("meta service error")
)

// Client is used to execute commands on and read data from
// a meta service cluster.
type Client struct {
	tls    bool
	logger *zap.Logger
	nodeID uint64

	mu          sync.RWMutex
	metaServers []string
	node        *freetsdb.Node
	changed     chan struct{}
	closing     chan struct{}
	cacheData   *Data

	// Authentication cache.
	authCache map[string]authUser
}

type authUser struct {
	bhash string
	salt  []byte
	hash  []byte
}

// NewClient returns a new *Client.
func NewClient(n *freetsdb.Node) *Client {
	return &Client{
		cacheData: &Data{
			ClusterID: uint64(rand.Int63()),
			Index:     1,
		},
		closing:   make(chan struct{}),
		changed:   make(chan struct{}),
		logger:    zap.NewNop(),
		authCache: make(map[string]authUser),

		node: n,
	}
}

// Open a connection to a meta service cluster.
func (c *Client) Open() error {

	c.changed = make(chan struct{})
	c.closing = make(chan struct{})
	c.cacheData = c.retryUntilSnapshot(0)

	go c.pollForUpdates()

	return nil
}

// Close the meta service cluster connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}

	select {
	case <-c.closing:
		return nil
	default:
		close(c.closing)
	}

	return nil
}

// GetNodeID returns the client's node ID.
func (c *Client) NodeID() uint64 { return c.nodeID }

// SetMetaServers updates the meta servers on the client.
func (c *Client) SetMetaServers(a []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metaServers = a
}

// SetTLS sets whether the client should use TLS when connecting.
// This function is not safe for concurrent use.
func (c *Client) SetTLS(v bool) { c.tls = v }

// Ping will hit the ping endpoint for the metaservice and return nil if
// it returns 200. If checkAllMetaServers is set to true, it will hit the
// ping endpoint and tell it to verify the health of all metaservers in the
// cluster
func (c *Client) Ping(checkAllMetaServers bool) error {
	c.mu.RLock()
	server := c.metaServers[0]
	c.mu.RUnlock()
	url := c.url(server) + "/ping"
	if checkAllMetaServers {
		url = url + "?all=true"
	}

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return nil
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return fmt.Errorf(string(b))
}

// AcquireLease attempts to acquire the specified lease.
// A lease is a logical concept that can be used by anything that needs to limit
// execution to a single node.  E.g., the CQ service on all nodes may ask for
// the "ContinuousQuery" lease. Only the node that acquires it will run CQs.
// NOTE: Leases are not managed through the CP system and are not fully
// consistent.  Any actions taken after acquiring a lease must be idempotent.
func (c *Client) AcquireLease(name string) (l *Lease, err error) {
	for n := 1; n < 11; n++ {
		if l, err = c.acquireLease(name); err == ErrServiceUnavailable || err == ErrService {
			// exponential backoff
			d := time.Duration(math.Pow(10, float64(n))) * time.Millisecond
			time.Sleep(d)
			continue
		}
		break
	}
	return
}

func (c *Client) acquireLease(name string) (*Lease, error) {
	c.mu.RLock()
	server := c.metaServers[0]
	c.mu.RUnlock()
	url := fmt.Sprintf("%s/lease?name=%s&nodeid=%d", c.url(server), name, c.nodeID)

	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusConflict:
		err = errors.New("another node owns the lease")
	case http.StatusServiceUnavailable:
		return nil, ErrServiceUnavailable
	case http.StatusBadRequest:
		b, e := ioutil.ReadAll(resp.Body)
		if e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("meta service: %s", string(b))
	case http.StatusInternalServerError:
		return nil, errors.New("meta service internal error")
	default:
		return nil, errors.New("unrecognized meta service error")
	}

	// Read lease JSON from response body.
	b, e := ioutil.ReadAll(resp.Body)
	if e != nil {
		return nil, e
	}
	// Unmarshal JSON into a Lease.
	l := &Lease{}
	if e = json.Unmarshal(b, l); e != nil {
		return nil, e
	}

	return l, err
}

func (c *Client) data() *Data {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cacheData
}

// ClusterID returns the ID of the cluster it's connected to.
func (c *Client) ClusterID() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.cacheData.ClusterID
}

// Node returns a node by id.
func (c *Client) DataNode(id uint64) (*NodeInfo, error) {
	for _, n := range c.data().DataNodes {
		if n.ID == id {
			return &n, nil
		}
	}
	return nil, ErrNodeNotFound
}

// DataNodes returns the data nodes' info.
func (c *Client) DataNodes() ([]NodeInfo, error) {
	return c.data().DataNodes, nil
}

// CreateDataNode will create a new data node in the metastore
func (c *Client) CreateDataNode(httpAddr, tcpAddr string) (*NodeInfo, error) {
	cmd := &internal.CreateDataNodeCommand{
		HTTPAddr: proto.String(httpAddr),
		TCPAddr:  proto.String(tcpAddr),
	}

	if err := c.retryUntilExec(internal.Command_CreateDataNodeCommand, internal.E_CreateDataNodeCommand_Command, cmd); err != nil {
		return nil, err
	}

	n, err := c.DataNodeByTCPHost(tcpAddr)
	if err != nil {
		return nil, err
	}

	c.nodeID = n.ID

	return n, nil
}

// DataNodeByHTTPHost returns the data node with the give http bind address
func (c *Client) DataNodeByHTTPHost(httpAddr string) (*NodeInfo, error) {
	nodes, _ := c.DataNodes()
	for _, n := range nodes {
		if n.Host == httpAddr {
			return &n, nil
		}
	}

	return nil, ErrNodeNotFound
}

// DataNodeByTCPHost returns the data node with the give http bind address
func (c *Client) DataNodeByTCPHost(tcpAddr string) (*NodeInfo, error) {

	nodes, _ := c.DataNodes()
	for _, n := range nodes {
		if n.TCPHost == tcpAddr {
			return &n, nil
		}
	}

	return nil, ErrNodeNotFound
}

// DeleteDataNode deletes a data node from the cluster.
func (c *Client) DeleteDataNode(id uint64) error {
	cmd := &internal.DeleteDataNodeCommand{
		ID: proto.Uint64(id),
	}

	return c.retryUntilExec(internal.Command_DeleteDataNodeCommand, internal.E_DeleteDataNodeCommand_Command, cmd)
}

// MetaNodes returns the meta nodes' info.
func (c *Client) MetaNodes() ([]NodeInfo, error) {
	return c.data().MetaNodes, nil
}

// MetaNodeByAddr returns the meta node's info.
func (c *Client) MetaNodeByAddr(addr string) *NodeInfo {
	for _, n := range c.data().MetaNodes {
		if n.Host == addr {
			return &n
		}
	}
	return nil
}

// Database returns info for the requested database.
func (c *Client) Database(name string) *DatabaseInfo {
	for _, d := range c.data().Databases {
		if d.Name == name {
			return &d
		}
	}

	// Can't throw ErrDatabaseNotExists here since it would require some major
	// work around catching the error when needed. Should be revisited.
	return nil
}

// Databases returns a list of all database infos.
func (c *Client) Databases() ([]DatabaseInfo, error) {
	dbs := c.data().Databases
	if dbs == nil {
		return []DatabaseInfo{}, nil
	}
	return dbs, nil
}

// CreateDatabase creates a database or returns it if it already exists
func (c *Client) CreateDatabase(name string) (*DatabaseInfo, error) {
	if db := c.Database(name); db != nil {
		return db, nil
	}

	cmd := &internal.CreateDatabaseCommand{
		Name: proto.String(name),
	}

	err := c.retryUntilExec(internal.Command_CreateDatabaseCommand, internal.E_CreateDatabaseCommand_Command, cmd)
	if err != nil {
		return nil, err
	}

	return c.Database(name), err
}

// CreateDatabaseWithRetentionPolicy creates a database with the specified retention policy.
func (c *Client) CreateDatabaseWithRetentionPolicy(name string, rpi *RetentionPolicyInfo) (*DatabaseInfo, error) {
	if rpi.Duration < MinRetentionPolicyDuration && rpi.Duration != 0 {
		return nil, ErrRetentionPolicyDurationTooLow
	}

	if db := c.Database(name); db != nil {
		// Check if the retention policy already exists. If it does and matches
		// the desired retention policy, exit with no error.
		if rp := db.RetentionPolicy(rpi.Name); rp != nil {
			if rp.ReplicaN != rpi.ReplicaN || rp.Duration != rpi.Duration {
				return nil, ErrRetentionPolicyConflict
			}
			return db, nil
		}
	}

	cmd := &internal.CreateDatabaseCommand{
		Name:            proto.String(name),
		RetentionPolicy: rpi.marshal(),
	}

	err := c.retryUntilExec(internal.Command_CreateDatabaseCommand, internal.E_CreateDatabaseCommand_Command, cmd)
	if err != nil {
		return nil, err
	}

	return c.Database(name), err
}

// DropDatabase deletes a database.
func (c *Client) DropDatabase(name string) error {
	cmd := &internal.DropDatabaseCommand{
		Name: proto.String(name),
	}

	return c.retryUntilExec(internal.Command_DropDatabaseCommand, internal.E_DropDatabaseCommand_Command, cmd)
}

// CreateRetentionPolicy creates a retention policy on the specified database.
func (c *Client) CreateRetentionPolicy(database string, rpi *RetentionPolicyInfo) (*RetentionPolicyInfo, error) {

	if rp, _ := c.RetentionPolicy(database, rpi.Name); rp != nil {
		return rp, nil
	}

	if rpi.Duration < MinRetentionPolicyDuration && rpi.Duration != 0 {
		return nil, ErrRetentionPolicyDurationTooLow
	}

	cmd := &internal.CreateRetentionPolicyCommand{
		Database:        proto.String(database),
		RetentionPolicy: rpi.marshal(),
	}

	if err := c.retryUntilExec(internal.Command_CreateRetentionPolicyCommand, internal.E_CreateRetentionPolicyCommand_Command, cmd); err != nil {
		return nil, err
	}

	return c.RetentionPolicy(database, rpi.Name)
}

// RetentionPolicy returns the requested retention policy info.
func (c *Client) RetentionPolicy(database, name string) (rpi *RetentionPolicyInfo, err error) {
	db := c.Database(database)

	// TODO: This should not be handled here
	if db == nil {
		return nil, freetsdb.ErrDatabaseNotFound(database)
	}

	return db.RetentionPolicy(name), nil
}

// DropRetentionPolicy drops a retention policy from a database.
func (c *Client) DropRetentionPolicy(database, name string) error {
	cmd := &internal.DropRetentionPolicyCommand{
		Database: proto.String(database),
		Name:     proto.String(name),
	}

	return c.retryUntilExec(internal.Command_DropRetentionPolicyCommand, internal.E_DropRetentionPolicyCommand_Command, cmd)
}

// SetDefaultRetentionPolicy sets a database's default retention policy.
func (c *Client) SetDefaultRetentionPolicy(database, name string) error {
	cmd := &internal.SetDefaultRetentionPolicyCommand{
		Database: proto.String(database),
		Name:     proto.String(name),
	}

	return c.retryUntilExec(internal.Command_SetDefaultRetentionPolicyCommand, internal.E_SetDefaultRetentionPolicyCommand_Command, cmd)
}

// UpdateRetentionPolicy updates a retention policy.
func (c *Client) UpdateRetentionPolicy(database, name string, rpu *RetentionPolicyUpdate) error {
	var newName *string
	if rpu.Name != nil {
		newName = rpu.Name
	}

	var duration *int64
	if rpu.Duration != nil {
		value := int64(*rpu.Duration)
		duration = &value
	}

	var replicaN *uint32
	if rpu.ReplicaN != nil {
		value := uint32(*rpu.ReplicaN)
		replicaN = &value
	}

	cmd := &internal.UpdateRetentionPolicyCommand{
		Database: proto.String(database),
		Name:     proto.String(name),
		NewName:  newName,
		Duration: duration,
		ReplicaN: replicaN,
	}

	return c.retryUntilExec(internal.Command_UpdateRetentionPolicyCommand, internal.E_UpdateRetentionPolicyCommand_Command, cmd)
}

// IsLeader - should get rid of this
func (c *Client) IsLeader() bool {
	return false
}

// Users returns a slice of UserInfo representing the currently known users.
func (c *Client) Users() []UserInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	users := c.cacheData.Users

	if users == nil {
		return []UserInfo{}
	}
	return users
}

func (c *Client) user(name string) (*UserInfo, error) {
	for _, u := range c.data().Users {
		if u.Name == name {
			return &u, nil
		}
	}

	return nil, ErrUserNotFound
}

// User returns the user with the given name, or ErrUserNotFound.
func (c *Client) User(name string) (User, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, u := range c.cacheData.Users {
		if u.Name == name {
			return &u, nil
		}
	}

	return nil, ErrUserNotFound
}

// bcryptCost is the cost associated with generating password with bcrypt.
// This setting is lowered during testing to improve test suite performance.
var bcryptCost = bcrypt.DefaultCost

// hashWithSalt returns a salted hash of password using salt.
func (c *Client) hashWithSalt(salt []byte, password string) []byte {
	hasher := sha256.New()
	hasher.Write(salt)
	hasher.Write([]byte(password))
	return hasher.Sum(nil)
}

// saltedHash returns a salt and salted hash of password.
func (c *Client) saltedHash(password string) (salt, hash []byte, err error) {
	salt = make([]byte, SaltBytes)
	if _, err := io.ReadFull(crand.Reader, salt); err != nil {
		return nil, nil, err
	}

	return salt, c.hashWithSalt(salt, password), nil
}

func (c *Client) CreateUser(name, password string, admin bool) (*UserInfo, error) {
	data := c.cacheData.Clone()

	// See if the user already exists.
	if u := data.user(name); u != nil {
		if err := bcrypt.CompareHashAndPassword([]byte(u.Hash), []byte(password)); err != nil || u.Admin != admin {
			return nil, ErrUserExists
		}
		return u, nil
	}

	// Hash the password before serializing it.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, err
	}

	if err := c.retryUntilExec(internal.Command_CreateUserCommand, internal.E_CreateUserCommand_Command,
		&internal.CreateUserCommand{
			Name:  proto.String(name),
			Hash:  proto.String(string(hash)),
			Admin: proto.Bool(admin),
		},
	); err != nil {
		return nil, err
	}
	return c.user(name)
}

func (c *Client) UpdateUser(name, password string) error {
	// Hash the password before serializing it.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}

	return c.retryUntilExec(internal.Command_UpdateUserCommand, internal.E_UpdateUserCommand_Command,
		&internal.UpdateUserCommand{
			Name: proto.String(name),
			Hash: proto.String(string(hash)),
		},
	)
}

func (c *Client) DropUser(name string) error {
	return c.retryUntilExec(internal.Command_DropUserCommand, internal.E_DropUserCommand_Command,
		&internal.DropUserCommand{
			Name: proto.String(name),
		},
	)
}

func (c *Client) SetPrivilege(username, database string, p influxql.Privilege) error {
	return c.retryUntilExec(internal.Command_SetPrivilegeCommand, internal.E_SetPrivilegeCommand_Command,
		&internal.SetPrivilegeCommand{
			Username:  proto.String(username),
			Database:  proto.String(database),
			Privilege: proto.Int32(int32(p)),
		},
	)
}

func (c *Client) SetAdminPrivilege(username string, admin bool) error {
	return c.retryUntilExec(internal.Command_SetAdminPrivilegeCommand, internal.E_SetAdminPrivilegeCommand_Command,
		&internal.SetAdminPrivilegeCommand{
			Username: proto.String(username),
			Admin:    proto.Bool(admin),
		},
	)
}

func (c *Client) UserPrivileges(username string) (map[string]influxql.Privilege, error) {
	p, err := c.data().UserPrivileges(username)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (c *Client) UserPrivilege(username, database string) (*influxql.Privilege, error) {
	p, err := c.data().UserPrivilege(username, database)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (c *Client) AdminUserExists() bool {
	for _, u := range c.data().Users {
		if u.Admin {
			return true
		}
	}
	return false
}

// Authenticate returns a UserInfo if the username and password match an existing entry.
func (c *Client) Authenticate(username, password string) (User, error) {
	// Find user.
	c.mu.RLock()
	userInfo := c.cacheData.user(username)
	c.mu.RUnlock()
	if userInfo == nil {
		return nil, ErrUserNotFound
	}

	// Check the local auth cache first.
	c.mu.RLock()
	au, ok := c.authCache[username]
	c.mu.RUnlock()
	if ok {
		// verify the password using the cached salt and hash
		if bytes.Equal(c.hashWithSalt(au.salt, password), au.hash) {
			return userInfo, nil
		}

		// fall through to requiring a full bcrypt hash for invalid passwords
	}

	// Compare password with user hash.
	if err := bcrypt.CompareHashAndPassword([]byte(userInfo.Hash), []byte(password)); err != nil {
		return nil, ErrAuthenticate
	}

	// generate a salt and hash of the password for the cache
	salt, hashed, err := c.saltedHash(password)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.authCache[username] = authUser{salt: salt, hash: hashed, bhash: userInfo.Hash}
	c.mu.Unlock()
	return userInfo, nil
}

// UserCount returns the number of users stored.
func (c *Client) UserCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return len(c.cacheData.Users)
}

// ShardIDs returns a list of all shard ids.
func (c *Client) ShardIDs() []uint64 {
	c.mu.RLock()

	var a []uint64
	for _, dbi := range c.cacheData.Databases {
		for _, rpi := range dbi.RetentionPolicies {
			for _, sgi := range rpi.ShardGroups {
				for _, si := range sgi.Shards {
					a = append(a, si.ID)
				}
			}
		}
	}
	c.mu.RUnlock()
	sort.Sort(uint64Slice(a))
	return a
}

// ShardGroupsByTimeRange returns a list of all shard groups on a database and policy that may contain data
// for the specified time range. Shard groups are sorted by start time.
func (c *Client) ShardGroupsByTimeRange(database, policy string, min, max time.Time) (a []ShardGroupInfo, err error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Find retention policy.
	rpi, err := c.cacheData.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, freetsdb.ErrRetentionPolicyNotFound(policy)
	}
	groups := make([]ShardGroupInfo, 0, len(rpi.ShardGroups))
	for _, g := range rpi.ShardGroups {
		if g.Deleted() || !g.Overlaps(min, max) {
			continue
		}
		groups = append(groups, g)
	}
	return groups, nil
}

// ShardsByTimeRange returns a slice of shards that may contain data in the time range.
func (c *Client) ShardsByTimeRange(sources influxql.Sources, tmin, tmax time.Time) (a []ShardInfo, err error) {
	m := make(map[*ShardInfo]struct{})
	for _, mm := range sources.Measurements() {
		groups, err := c.ShardGroupsByTimeRange(mm.Database, mm.RetentionPolicy, tmin, tmax)
		if err != nil {
			return nil, err
		}
		for _, g := range groups {
			for i := range g.Shards {
				m[&g.Shards[i]] = struct{}{}
			}
		}
	}

	a = make([]ShardInfo, 0, len(m))
	for sh := range m {
		a = append(a, *sh)
	}

	return a, nil
}

// DropShard deletes a shard by ID.
func (c *Client) DropShard(id uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data := c.cacheData.Clone()
	data.DropShard(id)
	return c.commit(data)
}

// TruncateShardGroups truncates any shard group that could contain timestamps beyond t.
func (c *Client) TruncateShardGroups(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data := c.cacheData.Clone()
	data.TruncateShardGroups(t)
	return c.commit(data)
}

// PruneShardGroups remove deleted shard groups from the data store.
func (c *Client) PruneShardGroups() error {
	var changed bool
	expiration := time.Now().Add(ShardGroupDeletedExpiration)
	c.mu.Lock()
	defer c.mu.Unlock()
	data := c.cacheData.Clone()
	for i, d := range data.Databases {
		for j, rp := range d.RetentionPolicies {
			var remainingShardGroups []ShardGroupInfo
			for _, sgi := range rp.ShardGroups {
				if sgi.DeletedAt.IsZero() || !expiration.After(sgi.DeletedAt) {
					remainingShardGroups = append(remainingShardGroups, sgi)
					continue
				}
				changed = true
			}
			data.Databases[i].RetentionPolicies[j].ShardGroups = remainingShardGroups
		}
	}
	if changed {
		return c.commit(data)
	}
	return nil
}

// CreateShardGroup creates a shard group on a database and policy for a given timestamp.
func (c *Client) CreateShardGroup(database, policy string, timestamp time.Time) (*ShardGroupInfo, error) {
	if sg, _ := c.data().ShardGroupByTimestamp(database, policy, timestamp); sg != nil {
		return sg, nil
	}

	cmd := &internal.CreateShardGroupCommand{
		Database:  proto.String(database),
		Policy:    proto.String(policy),
		Timestamp: proto.Int64(timestamp.UnixNano()),
	}

	if err := c.retryUntilExec(internal.Command_CreateShardGroupCommand, internal.E_CreateShardGroupCommand_Command, cmd); err != nil {
		return nil, err
	}

	rpi, err := c.RetentionPolicy(database, policy)
	if err != nil {
		return nil, err
	} else if rpi == nil {
		return nil, errors.New("retention policy deleted after shard group created")
	}

	return rpi.ShardGroupByTimestamp(timestamp), nil
}

// DeleteShardGroup removes a shard group from a database and retention policy by id.
func (c *Client) DeleteShardGroup(database, policy string, id uint64) error {
	cmd := &internal.DeleteShardGroupCommand{
		Database:     proto.String(database),
		Policy:       proto.String(policy),
		ShardGroupID: proto.Uint64(id),
	}

	return c.retryUntilExec(internal.Command_DeleteShardGroupCommand, internal.E_DeleteShardGroupCommand_Command, cmd)
}

// PrecreateShardGroups creates shard groups whose endtime is before the 'to' time passed in, but
// is yet to expire before 'from'. This is to avoid the need for these shards to be created when data
// for the corresponding time range arrives. Shard creation involves Raft consensus, and precreation
// avoids taking the hit at write-time.
func (c *Client) PrecreateShardGroups(from, to time.Time) error {
	for _, di := range c.data().Databases {
		for _, rp := range di.RetentionPolicies {
			if len(rp.ShardGroups) == 0 {
				// No data was ever written to this group, or all groups have been deleted.
				continue
			}
			g := rp.ShardGroups[len(rp.ShardGroups)-1] // Get the last group in time.
			if !g.Deleted() && g.EndTime.Before(to) && g.EndTime.After(from) {
				// Group is not deleted, will end before the future time, but is still yet to expire.
				// This last check is important, so the system doesn't create shards groups wholly
				// in the past.

				// Create successive shard group.
				nextShardGroupTime := g.EndTime.Add(1 * time.Nanosecond)
				if newGroup, err := c.CreateShardGroup(di.Name, rp.Name, nextShardGroupTime); err != nil {
					c.logger.Info("Failed to precreate successive shard group",
						zap.Uint64("Group ID", g.ID),
						zap.Error(err))
				} else {
					c.logger.Info("New shard group successfully precreated",
						zap.Uint64("Group ID", newGroup.ID),
						zap.String("Database", di.Name),
						zap.String("Retention policy", rp.Name))
				}
			}
		}
		return nil
	}
	return nil
}

// ShardOwner returns the owning shard group info for a specific shard.
func (c *Client) ShardOwner(shardID uint64) (database, policy string, sgi *ShardGroupInfo) {
	for _, dbi := range c.data().Databases {
		for _, rpi := range dbi.RetentionPolicies {
			for _, g := range rpi.ShardGroups {
				if g.Deleted() {
					continue
				}

				for _, sh := range g.Shards {
					if sh.ID == shardID {
						database = dbi.Name
						policy = rpi.Name
						sgi = &g
						return
					}
				}
			}
		}
	}
	return
}

// JoinMetaServer will add the passed in tcpAddr to the raft peers and add a MetaNode to
// the metastore
func (c *Client) JoinMetaServer(httpAddr, tcpAddr string) (*NodeInfo, error) {
	node := &NodeInfo{
		Host:    httpAddr,
		TCPHost: tcpAddr,
	}
	b, err := json.Marshal(node)
	if err != nil {
		return nil, err
	}

	currentServer := 0
	redirectServer := ""
	for {
		// get the server to try to join against
		var url string
		if redirectServer != "" {
			url = redirectServer
			redirectServer = ""
		} else {
			c.mu.RLock()

			if currentServer >= len(c.metaServers) {
				// We've tried every server, wait a second before
				// trying again
				time.Sleep(time.Second)
				currentServer = 0
			}
			server := c.metaServers[currentServer]
			c.mu.RUnlock()

			url = c.url(server) + "/add-meta"
		}

		resp, err := http.Post(url, "application/json", bytes.NewBuffer(b))
		if err != nil {
			currentServer++
			continue
		}

		// Successfully joined
		if resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
			if err := json.NewDecoder(resp.Body).Decode(&node); err != nil {
				return nil, err
			}
			break
		}
		resp.Body.Close()

		// We tried to join a meta node that was not the leader, rety at the node
		// they think is the leader.
		if resp.StatusCode == http.StatusTemporaryRedirect {
			redirectServer = resp.Header.Get("Location")
			continue
		}

		// Something failed, try the next node
		currentServer++
	}

	return node, nil
}

func (c *Client) CreateMetaNode(httpAddr, tcpAddr string) (*NodeInfo, error) {
	cmd := &internal.CreateMetaNodeCommand{
		HTTPAddr: proto.String(httpAddr),
		TCPAddr:  proto.String(tcpAddr),
		Rand:     proto.Uint64(uint64(rand.Int63())),
	}

	if err := c.retryUntilExec(internal.Command_CreateMetaNodeCommand, internal.E_CreateMetaNodeCommand_Command, cmd); err != nil {
		return nil, err
	}

	n := c.MetaNodeByAddr(httpAddr)
	if n == nil {
		return nil, errors.New("new meta node not found")
	}

	c.nodeID = n.ID

	return n, nil
}

func (c *Client) DeleteMetaNode(id uint64) error {
	cmd := &internal.DeleteMetaNodeCommand{
		ID: proto.Uint64(id),
	}

	return c.retryUntilExec(internal.Command_DeleteMetaNodeCommand, internal.E_DeleteMetaNodeCommand_Command, cmd)
}

func (c *Client) CreateContinuousQuery(database, name, query string) error {
	return c.retryUntilExec(internal.Command_CreateContinuousQueryCommand, internal.E_CreateContinuousQueryCommand_Command,
		&internal.CreateContinuousQueryCommand{
			Database: proto.String(database),
			Name:     proto.String(name),
			Query:    proto.String(query),
		},
	)
}

func (c *Client) DropContinuousQuery(database, name string) error {
	return c.retryUntilExec(internal.Command_DropContinuousQueryCommand, internal.E_DropContinuousQueryCommand_Command,
		&internal.DropContinuousQueryCommand{
			Database: proto.String(database),
			Name:     proto.String(name),
		},
	)
}

func (c *Client) CreateSubscription(database, rp, name, mode string, destinations []string) error {
	return c.retryUntilExec(internal.Command_CreateSubscriptionCommand, internal.E_CreateSubscriptionCommand_Command,
		&internal.CreateSubscriptionCommand{
			Database:        proto.String(database),
			RetentionPolicy: proto.String(rp),
			Name:            proto.String(name),
			Mode:            proto.String(mode),
			Destinations:    destinations,
		},
	)
}

func (c *Client) DropSubscription(database, rp, name string) error {
	return c.retryUntilExec(internal.Command_DropSubscriptionCommand, internal.E_DropSubscriptionCommand_Command,
		&internal.DropSubscriptionCommand{
			Database:        proto.String(database),
			RetentionPolicy: proto.String(rp),
			Name:            proto.String(name),
		},
	)
}

func (c *Client) SetData(data *Data) error {
	return c.retryUntilExec(internal.Command_SetDataCommand, internal.E_SetDataCommand_Command,
		&internal.SetDataCommand{
			Data: data.marshal(),
		},
	)
}

// Data returns a clone of the underlying data in the meta store.
func (c *Client) Data() Data {
	c.mu.RLock()
	defer c.mu.RUnlock()
	d := c.cacheData.Clone()
	return *d
}

// WaitForDataChanged returns a channel that will get closed when
// the metastore data has changed.
func (c *Client) WaitForDataChanged() chan struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.changed
}

// commit writes data to the underlying store.
// This method assumes c's mutex is already locked.
func (c *Client) commit(data *Data) error {

	data.Index++

	// try to write to disk before updating in memory
	if c.node != nil {
		if err := snapshot(c.node.Path, data); err != nil {
			return err
		}
	}

	// update in memory
	c.cacheData = data

	// close channels to signal changes
	close(c.changed)
	c.changed = make(chan struct{})

	return nil
}

// MarshalBinary returns a binary representation of the underlying data.
func (c *Client) MarshalBinary() ([]byte, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cacheData.MarshalBinary()
}

// WithLogger sets the logger for the client.
func (c *Client) WithLogger(log *zap.Logger) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logger = log.With(zap.String("service", "metaclient"))
}

// snapshot saves the current meta data to disk.
func snapshot(path string, data *Data) error {
	filename := filepath.Join(path, metaFile)
	tmpFile := filename + "tmp"

	f, err := os.Create(tmpFile)
	if err != nil {
		return err
	}
	defer f.Close()

	var d []byte
	if b, err := data.MarshalBinary(); err != nil {
		return err
	} else {
		d = b
	}

	if _, err := f.Write(d); err != nil {
		return err
	}

	if err = f.Sync(); err != nil {
		return err
	}

	//close file handle before renaming to support Windows
	if err = f.Close(); err != nil {
		return err
	}

	return file.RenameFile(tmpFile, filename)
}

// Load loads the current meta data from disk.
func (c *Client) Load() error {
	if c.node == nil {
		return nil
	}

	file := filepath.Join(c.node.Path, metaFile)

	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	data, err := ioutil.ReadAll(f)
	if err != nil {
		return err
	}

	if err := c.cacheData.UnmarshalBinary(data); err != nil {
		return err
	}
	return nil
}

func (c *Client) index() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cacheData.Index
}

// retryUntilExec will attempt the command on each of the metaservers until it either succeeds or
// hits the max number of tries
func (c *Client) retryUntilExec(typ internal.Command_Type, desc *proto.ExtensionDesc, value interface{}) error {
	var err error
	var index uint64
	tries := 0
	currentServer := 0
	var redirectServer string

	for {
		c.mu.RLock()
		// exit if we're closed
		select {
		case <-c.closing:
			c.mu.RUnlock()
			return nil
		default:
			// we're still open, continue on
		}
		c.mu.RUnlock()

		// build the url to hit the redirect server or the next metaserver
		var url string
		if redirectServer != "" {
			url = redirectServer
			redirectServer = ""
		} else {
			c.mu.RLock()
			if currentServer >= len(c.metaServers) {
				currentServer = 0
			}
			server := c.metaServers[currentServer]
			c.mu.RUnlock()

			url = fmt.Sprintf("://%s/execute", server)
			if c.tls {
				url = "https" + url
			} else {
				url = "http" + url
			}
		}

		index, err = c.exec(url, typ, desc, value)
		tries++
		currentServer++

		if err == nil {
			c.waitForIndex(index)
			return nil
		}

		if tries > maxRetries {
			return err
		}

		if e, ok := err.(errRedirect); ok {
			redirectServer = e.host
			continue
		}

		if _, ok := err.(errCommand); ok {
			return err
		}

		time.Sleep(errSleep)
	}
}

func (c *Client) exec(url string, typ internal.Command_Type, desc *proto.ExtensionDesc, value interface{}) (index uint64, err error) {
	// Create command.
	cmd := &internal.Command{Type: &typ}
	if err := proto.SetExtension(cmd, desc, value); err != nil {
		panic(err)
	}

	b, err := proto.Marshal(cmd)
	if err != nil {
		return 0, err
	}

	resp, err := http.Post(url, "application/octet-stream", bytes.NewBuffer(b))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// read the response
	if resp.StatusCode == http.StatusTemporaryRedirect {
		return 0, errRedirect{host: resp.Header.Get("Location")}
	} else if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("meta service returned %s", resp.Status)
	}

	res := &internal.Response{}

	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	if err := proto.Unmarshal(b, res); err != nil {
		return 0, err
	}
	es := res.GetError()
	if es != "" {
		return 0, errCommand{msg: es}
	}

	return res.GetIndex(), nil
}

func (c *Client) waitForIndex(idx uint64) {
	for {
		c.mu.RLock()
		if c.cacheData.Index >= idx {
			c.mu.RUnlock()
			return
		}
		ch := c.changed
		c.mu.RUnlock()
		<-ch
	}
}

func (c *Client) pollForUpdates() {
	for {
		data := c.retryUntilSnapshot(c.index())
		if data == nil {
			// this will only be nil if the client has been closed,
			// so we can exit out
			return
		}

		// update the data and notify of the change
		c.mu.Lock()
		idx := c.cacheData.Index
		c.cacheData = data
		c.updateAuthCache()
		c.updateMetaPeers()
		if idx < data.Index {
			close(c.changed)
			c.changed = make(chan struct{})
		}
		c.mu.Unlock()
	}
}

func (c *Client) getSnapshot(server string, index uint64) (*Data, error) {
	resp, err := http.Get(c.url(server) + fmt.Sprintf("?index=%d", index))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("meta server returned non-200: %s", resp.Status)
	}

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	data := &Data{}
	if err := data.UnmarshalBinary(b); err != nil {
		return nil, err
	}

	return data, nil
}

// peers returns the TCPHost addresses of all the metaservers
func (c *Client) peers() []string {

	var peers Peers
	// query each server and keep track of who their peers are
	for _, server := range c.metaServers {
		url := c.url(server) + "/peers"
		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		// This meta-server might not be ready to answer, continue on
		if resp.StatusCode != http.StatusOK {
			continue
		}

		dec := json.NewDecoder(resp.Body)
		var p []string
		if err := dec.Decode(&p); err != nil {
			continue
		}
		peers = peers.Append(p...)
	}

	// Return the unique set of peer addresses
	return []string(peers.Unique())
}

func (c *Client) url(server string) string {
	url := fmt.Sprintf("://%s", server)

	if c.tls {
		url = "https" + url
	} else {
		url = "http" + url
	}

	return url
}

func (c *Client) retryUntilSnapshot(idx uint64) *Data {
	currentServer := 0
	for {
		// get the index to look from and the server to poll
		c.mu.RLock()

		// exit if we're closed
		select {
		case <-c.closing:
			c.mu.RUnlock()
			return nil
		default:
			// we're still open, continue on
		}

		if currentServer >= len(c.metaServers) {
			currentServer = 0
		}
		server := c.metaServers[currentServer]
		c.mu.RUnlock()

		data, err := c.getSnapshot(server, idx)

		if err == nil {
			return data
		}

		c.logger.Info("Failed to get snapshot from %s: %s",
			zap.String("server", server), zap.Error(err))

		time.Sleep(errSleep)

		currentServer++
	}
}

func (c *Client) updateAuthCache() {
	// copy cached user info for still-present users
	newCache := make(map[string]authUser, len(c.authCache))

	for _, userInfo := range c.cacheData.Users {
		if cached, ok := c.authCache[userInfo.Name]; ok {
			if cached.bhash == userInfo.Hash {
				newCache[userInfo.Name] = cached
			}
		}
	}

	c.authCache = newCache
}

func (c *Client) updateMetaPeers() {

	if c.node == nil {
		return
	}

	peers := []string{}
	for _, metaNode := range c.cacheData.MetaNodes {
		peers = append(peers, metaNode.Host)
	}

	if !reflect.DeepEqual(c.node.Peers, peers) {
		c.metaServers = peers
		oldPeers := c.node.Peers
		c.node.Peers = peers
		if err := c.node.Save(); err != nil {
			c.node.Peers = oldPeers
			c.logger.Info("Error save node: %s", zap.Error(err))
			return
		}
	}
}

func (c *Client) MetaServers() []string {
	return c.metaServers
}

type Peers []string

func (peers Peers) Append(p ...string) Peers {
	peers = append(peers, p...)

	return peers.Unique()
}

func (peers Peers) Unique() Peers {
	distinct := map[string]struct{}{}
	for _, p := range peers {
		distinct[p] = struct{}{}
	}

	var u Peers
	for k := range distinct {
		u = append(u, k)
	}
	return u
}

func (peers Peers) Contains(peer string) bool {
	for _, p := range peers {
		if p == peer {
			return true
		}
	}
	return false
}

type errRedirect struct {
	host string
}

func (e errRedirect) Error() string {
	return fmt.Sprintf("redirect to %s", e.host)
}

type errCommand struct {
	msg string
}

func (e errCommand) Error() string {
	return e.msg
}

type uint64Slice []uint64

func (a uint64Slice) Len() int           { return len(a) }
func (a uint64Slice) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a uint64Slice) Less(i, j int) bool { return a[i] < a[j] }
