package tsdb

import (
	"github.com/freetsdb/freetsdb/platform/query"
)

// EOF represents a "not found" key returned by a Cursor.
const EOF = query.ZeroTime
