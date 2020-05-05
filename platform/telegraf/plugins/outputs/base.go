package outputs

import "github.com/freetsdb/freetsdb/platform/telegraf/plugins"

type baseOutput int

func (b baseOutput) Type() plugins.Type {
	return plugins.Output
}
