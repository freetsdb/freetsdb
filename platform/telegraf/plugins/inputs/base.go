package inputs

import "github.com/freetsdb/freetsdb/platform/telegraf/plugins"

type baseInput int

func (b baseInput) Type() plugins.Type {
	return plugins.Input
}
