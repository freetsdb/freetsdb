package tsdb

import "github.com/freetsdb/freetsdb/models"

// Executor is an interface for a query executor.
type Executor interface {
	Execute(closing <-chan struct{}) <-chan *models.Row
}
