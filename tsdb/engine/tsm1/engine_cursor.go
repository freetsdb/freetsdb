package tsm1

import (
	"context"

	"github.com/freetsdb/freetsdb/tsdb"
)

func (e *Engine) CreateCursorIterator(ctx context.Context) (tsdb.CursorIterator, error) {
	return &arrayCursorIterator{e: e}, nil
}
