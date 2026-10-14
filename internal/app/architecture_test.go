package app

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Keep transport, persistence and concrete adapters out of business code.
func TestArchitectureImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !strings.Contains(strings.Split(path, "/")[0], ".") {
				continue
			}
			if path != "go.xchunk.org/vela/api" && path != "go.xchunk.org/vela-mcp/internal/access" {
				t.Errorf("business code %s imports infrastructure %s", e.Name(), path)
			}
		}
	}
}
