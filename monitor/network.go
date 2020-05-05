package monitor

import (
	"os"

	"github.com/freetsdb/freetsdb/monitor/diagnostics"
)

// network captures network diagnostics
type network struct{}

func (n *network) Diagnostics() (*diagnostics.Diagnostics, error) {
	h, err := os.Hostname()
	if err != nil {
		return nil, err
	}

	diagnostics := map[string]interface{}{
		"hostname": h,
	}

	return DiagnosticsFromMap(diagnostics), nil
}
