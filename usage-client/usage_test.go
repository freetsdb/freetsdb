package client_test

import (
	"fmt"
	"io/ioutil"
	"testing"

	"github.com/freetsdb/freetsdb/usage-client"
	"github.com/stretchr/testify/require"
)

func Test_Usage_Path(t *testing.T) {
	r := require.New(t)
	u := client.Usage{Product: "freetsdb"}
	r.Equal("/usage/freetsdb", u.Path())
}

// Example of saving Usage data to the Usage API
func Example_saveUsage() {
	c := client.New("token-goes-here")
	// override the URL for testing
	c.URL = "https://usage.staging.freetsdb.org"

	u := client.Usage{
		Product: "freetsdb",
		Data: []client.UsageData{
			{
				Tags: client.Tags{
					"version": "0.9.5",
					"arch":    "amd64",
					"os":      "linux",
				},
				Values: client.Values{
					"cluster_id":       "23423",
					"server_id":        "1",
					"num_databases":    3,
					"num_measurements": 2342,
					"num_series":       87232,
				},
			},
		},
	}

	res, err := c.Save(u)
	fmt.Printf("err: %s\n", err)
	b, _ := ioutil.ReadAll(res.Body)
	fmt.Printf("b: %s\n", b)
}
