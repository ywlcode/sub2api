package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Rebase guard: a newly added gateway consumer must read the installed replica
// policy. Resource-only fields are deliberately absent from the synchronized set.
func TestSitePolicyConsumersUseEffectiveGateway(t *testing.T) {
	paths := append([]string(nil), siteGatewayPolicyFields...)
	for path := range siteOptionalGatewayPolicyFields {
		paths = append(paths, path)
	}
	goPaths := map[string]bool{}
	for _, path := range paths {
		typ := reflect.TypeOf(GatewayConfig{})
		var names []string
		for _, part := range strings.Split(path, ".") {
			found := false
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				if field.Tag.Get("mapstructure") == part {
					names = append(names, field.Name)
					typ = field.Type
					found = true
					break
				}
			}
			require.True(t, found, path)
		}
		goPaths[strings.Join(names, ".")] = true
	}
	fset := token.NewFileSet()
	for _, dir := range []string{"../handler", "../service", "../server"} {
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				parts := []string{selector.Sel.Name}
				for expr := selector.X; expr != nil; {
					parent, ok := expr.(*ast.SelectorExpr)
					if !ok {
						break
					}
					if parent.Sel.Name == "Gateway" && goPaths[strings.Join(parts, ".")] {
						t.Errorf("%s: synchronized Gateway.%s must use RequestGateway()", fset.Position(selector.Pos()), strings.Join(parts, "."))
					}
					parts = append([]string{parent.Sel.Name}, parts...)
					expr = parent.X
				}
				return true
			})
			return nil
		})
		require.NoError(t, err)
	}
}
