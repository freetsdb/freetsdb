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

	Cmd         string
	MetaAddr    string
	DataAddr    string
	NewNodeAddr string

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
		return cmd.addMeta(cmd.MetaAddr, cmd.NewNodeAddr)
	} else if cmd.Cmd == "add-data" {
		return cmd.addData(cmd.MetaAddr, cmd.NewNodeAddr)
	} else if cmd.Cmd == "show" {
		return cmd.nodeInfo(cmd.MetaAddr)
	}
	return nil
}

// parseFlags parses and validates the command line arguments.
func (cmd *Command) parseFlags(args []string) error {

	if cmd.Cmd == "add-meta" && len(args) > 0 {
		cmd.NewNodeAddr = args[0]
	} else if cmd.Cmd == "add-data" && len(args) > 0 {
		cmd.NewNodeAddr = args[0]
	} else if cmd.Cmd == "show" {

	} else if cmd.Cmd == "freetsd-ctl" && len(args) > 0 && args[0] == "-h" {
		cmd.printUsage()
	} else {
		cmd.printUsage()
	}

	cmd.MetaAddr = "localhost:8091"

	// validate the arguments
	if (cmd.Cmd == "add-meta" || cmd.Cmd == "add-data") && cmd.NewNodeAddr == "" {
		return fmt.Errorf("new node address required")
	}

	return nil
}

func (cmd *Command) addMeta(metaAddr, newNodeAddr string) error {
	//func (c *Client) JoinMetaServer(httpAddr, tcpAddr string) (*NodeInfo, error) {

	resp, err := http.Get(fmt.Sprintf("http://%s/meta-servers", metaAddr))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf(string(b))
	}

	resp2, err := http.Post(fmt.Sprintf("http://%s/join-cluster", newNodeAddr),
		"application/json",
		bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	defer resp2.Body.Close()

	// Successfully joined
	node := meta.NodeInfo{}
	if resp2.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp2.Body).Decode(&node); err != nil {
			return err
		}

	}

	fmt.Printf("Added meta node %d at %s\n", node.ID, node.Host)

	return nil
}

const NodeMuxHeader = 9
const MetaServerInfo = 0x01

type Request struct {
	Type  uint8
	Peers []string
}

func (cmd *Command) addData(metaAddr, newNodeAddr string) error {

	resp, err := http.Get(fmt.Sprintf("http://%s/meta-servers", metaAddr))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	r := Request{}
	r.Type = MetaServerInfo
	if err := json.NewDecoder(resp.Body).Decode(&r.Peers); err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		return fmt.Errorf(string(b))
	}

	// Connect to snapshotter service.
	conn, err := tcp.Dial("tcp", newNodeAddr, NodeMuxHeader)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Write the request
	if err := json.NewEncoder(conn).Encode(r); err != nil {
		return fmt.Errorf("Encode snapshot request: %s", err)
	}

	// Read the response
	node := meta.NodeInfo{}
	if err := json.NewDecoder(conn).Decode(&node); err != nil {
		return err
	}

	fmt.Printf("Added data node %d at %s\n", node.ID, node.TCPHost)

	return nil
}

func (cmd *Command) nodeInfo(metaAddr string) error {

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
