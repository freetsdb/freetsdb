package parser

import (
	"github.com/freetsdb/freetsdb/services/flux/ast"
	fastparser "github.com/freetsdb/freetsdb/services/flux/internal/parser"
)

// NewAST parses Flux query and produces an ast.Program
func NewAST(flux string) (*ast.Program, error) {
	return fastparser.NewAST([]byte(flux)), nil
}
