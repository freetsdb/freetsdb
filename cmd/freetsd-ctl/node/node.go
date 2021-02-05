package node

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/freetsdb/freetsdb/services/meta"
	"github.com/freetsdb/freetsdb/tcp"
)

// Command represents the program execution for "freetsd restore".
type Command struct {
	Stdout io.Writer
	Stderr io.Writer

	Cmd            string
	MetaAddr       string
	RemoteNodeAddr string

	// TODO: when the new meta stuff is done this should not be exported or be gone
	MetaConfig *meta.Config
}

// NewCommand returns a new instance of Command with default settings.
func NewCommand(c string) *Command {
	return &Command{
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		Cmd:        c,
		MetaConfig: meta.NewConfig(),
	}
}

// Run executes the program.
func (cmd *Command) Run(args ...string) error {
	if err := cmd.parseFlags(args); err != nil {
		return err
	}

	if cmd.Cmd == "add-meta" {
		return cmd.addMeta(cmd.MetaAddr, cmd.RemoteNodeAddr)
	} else if cmd.Cmd == "remove-meta" {
		return cmd.removeMeta(cmd.MetaAddr, cmd.RemoteNodeAddr)
	} else if cmd.Cmd == "add-data" {
		return cmd.addData(cmd.MetaAddr, cmd.RemoteNodeAddr)
	} else if cmd.Cmd == "remove-data" {
		return cmd.removeData(cmd.MetaAddr, cmd.RemoteNodeAddr)
	} else if cmd.Cmd == "show" {
		return cmd.nodeInfo(cmd.MetaAddr)
	}

	return nil
}

// parseFlags parses and validates the command line arguments.
func (cmd *Command) parseFlags(args []string) error {

	if cmd.Cmd == "add-meta" && len(args) > 0 {
		cmd.RemoteNodeAddr = args[0]
	} else if cmd.Cmd == "remove-meta" && len(args) > 0 {
		cmd.RemoteNodeAddr = args[0]
	} else if cmd.Cmd == "add-data" && len(args) > 0 {
		cmd.RemoteNodeAddr = args[0]
	} else if cmd.Cmd == "remove-data" && len(args) > 0 {
		cmd.RemoteNodeAddr = args[0]
	} else if cmd.Cmd == "show" {

	} else if cmd.Cmd == "freetsd-ctl" && len(args) > 0 && args[0] == "-h" {
		cmd.printUsage()
	} else {
		cmd.printUsage()
	}

	if cmd.MetaAddr == "" {
		cmd.MetaAddr = "localhost:8091"
	}

	// validate the arguments
	if (cmd.Cmd == "add-meta" ||
		cmd.Cmd == "remove-meta" ||
		cmd.Cmd == "add-data" ||
		cmd.Cmd == "remove-data") &&
		cmd.RemoteNodeAddr == "" {
		return fmt.Errorf("Remote node address required")
	}

	return nil
}

func (cmd *Command) getMetaServers(metaAddr string) ([]string, error) {
	resp, err := http.Get(fmt.Sprintf("http://%s/meta-servers", metaAddr))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(string(b))
	}

	peers := []string{}
	if err := json.NewDecoder(resp.Body).Decode(&peers); err != nil {
		return nil, err
	}

	return peers, nil
}

func (cmd *Command) addMeta(metaAddr, newNodeAddr string) error {
	peers, err := cmd.getMetaServers(metaAddr)
	if err != nil {
		return err
	}

	if len(peers) == 0 {
		return fmt.Errorf("Failed to get MetaServerInfo: empty Peers")
	}

	b, err := json.Marshal(peers)
	if err != nil {
		return err
	}

	resp, err := http.Post(fmt.Sprintf("http://%s/join-cluster", newNodeAddr),
		"application/json",
		bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	n := meta.NodeInfo{}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
			return err
		}

	}

	fmt.Printf("Added meta node %d at %s\n", n.ID, n.Host)

	return nil
}

func (cmd *Command) removeMeta(metaAddr, remoteNodeAddr string) error {

	n := meta.NodeInfo{}
	n.Host = remoteNodeAddr

	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	resp, err := http.Post(fmt.Sprintf("http://%s/remove-meta", metaAddr),
		"application/json",
		bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
			return err
		}

	}

	fmt.Printf("Removed meta node %d at %s\n", n.ID, n.Host)

	return nil
}

const NodeMuxHeader = 9
const RequestClusterJoin = 0x01

type Request struct {
	Type  uint8
	Peers []string
}

func (cmd *Command) addData(metaAddr, newNodeAddr string) error {
	peers, err := cmd.getMetaServers(metaAddr)
	if err != nil {
		return err
	}

	if len(peers) == 0 {
		return fmt.Errorf("Failed to get MetaServerInfo: empty Peers")
	}

	r := Request{}
	r.Type = RequestClusterJoin
	r.Peers = peers

	conn, err := tcp.Dial("tcp", newNodeAddr, NodeMuxHeader)
	if err != nil {
		return err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(r); err != nil {
		return fmt.Errorf("Encode snapshot request: %s", err)
	}

	node := meta.NodeInfo{}
	if err := json.NewDecoder(conn).Decode(&node); err != nil {
		return err
	}

	fmt.Printf("Added data node %d at %s\n", node.ID, node.TCPHost)

	return nil
}

func (cmd *Command) removeData(metaAddr, remoteNodeAddr string) error {
	peers, err := cmd.getMetaServers(metaAddr)
	if err != nil {
		return err
	}

	if len(peers) == 0 {
		return fmt.Errorf("Failed to get MetaServerInfo: empty Peers")
	}

	metaClient := meta.NewClient(nil)
	metaClient.SetMetaServers(peers)
	if err := metaClient.Open(); err != nil {
		return err
	}
	defer metaClient.Close()

	n, err := metaClient.DataNodeByTCPHost(remoteNodeAddr)
	if err != nil {
		return err
	}

	if err := metaClient.DeleteDataNode(n.ID); err != nil {
		return err
	}

	fmt.Printf("Removed data node %d at %s\n", n.ID, n.TCPHost)
	return nil

}

func (cmd *Command) nodeInfo(metaAddr string) error {
	peers, err := cmd.getMetaServers(metaAddr)
	if err != nil {
		return err
	}

	if len(peers) == 0 {
		return fmt.Errorf("Failed to get MetaServerInfo: empty Peers")
	}

	metaClient := meta.NewClient(nil)
	metaClient.SetMetaServers(peers)
	if err := metaClient.Open(); err != nil {
		return err
	}
	defer metaClient.Close()

	metaNodes, err := metaClient.MetaNodes()
	if err != nil {
		return err
	}
	dataNodes, err := metaClient.DataNodes()
	if err != nil {
		return err
	}

	fmt.Fprintln(cmd.Stdout, "Data Nodes:")
	for _, n := range dataNodes {
		fmt.Fprintln(cmd.Stdout, n.ID, "    ", n.TCPHost)
	}
	fmt.Fprintln(cmd.Stdout, "")

	fmt.Fprintln(cmd.Stdout, "Meta Nodes:")
	for _, n := range metaNodes {
		fmt.Fprintln(cmd.Stdout, n.ID, "    ", n.Host)
	}
	fmt.Fprintln(cmd.Stdout, "")

	return nil
}

// printUsage prints the usage message to STDERR.
func (cmd *Command) printUsage() {
	fmt.Fprintf(cmd.Stdout, `usage: freetsd restore [flags] PATH

Restore uses backups from the PATH to restore the metastore, databases,
retention policies, or specific shards. The FreeTSDB process must not be
running during restore.

Options:
  -metadir <path>
        Optional. If set the metastore will be recovered to the given path.
  -datadir <path>
        Optional. If set the restore process will recover the specified
        database, retention policy or shard to the given directory.
  -database <name>
        Optional. Required if no metadir given. Will restore the database
        TSM files.
  -retention <name>
        Optional. If given, database is required. Will restore the retention policy's
        TSM files.
  -shard <id>
    Optional. If given, database and retention are required. Will restore the shard's
    TSM files.

`)
}
