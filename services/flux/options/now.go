package options

import (
	"github.com/freetsdb/freetsdb/services/flux"
	"github.com/freetsdb/freetsdb/services/flux/functions/transformations"
)

func init() {
	flux.RegisterBuiltInOption("now", transformations.SystemTime())
}
