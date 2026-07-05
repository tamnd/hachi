// Package hachi holds architecture tests: rules the codebase promises to
// keep, enforced in CI from the first commit.
package hachi

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

const module = "github.com/tamnd/hachi"

// allowedInternal says which hachi packages each package may import.
// The TUI is a pure client: hive and waggle only, never the engine,
// journal, or adapters. Adapters never reach up past waggle.
var allowedInternal = map[string][]string{
	"waggle":  {},
	"adapter": {"waggle"},
	"journal": {"waggle"},
	"hive":    {"waggle"},
	"engine":  {"adapter", "hive", "journal", "waggle"},
	"tui":     {"hive", "waggle"},
	"guard":   {"waggle"},
}

// allowedExternal is the dependency budget: the charm stack, cobra, and
// nothing else. Growing this list is a design decision, not a convenience.
var allowedExternal = []string{
	"charm.land/", // bubbletea v2 and friends
	"github.com/spf13/cobra",
	"golang.org/x/mod", // this test file only
}

func TestImportBoundaries(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		pkg := topPackage(path)
		if pkg == "" || pkg == "cmd" { // cmd wires everything; root is tests
			return nil
		}
		allowed, known := allowedInternal[pkg]
		if !known {
			t.Errorf("%s: package %q has no import contract; add it to allowedInternal", path, pkg)
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(ip, module+"/") {
				continue
			}
			dep := topPackage(strings.TrimPrefix(ip, module+"/") + "/x.go")
			ok := dep == pkg // a tree may import within itself
			for _, a := range allowed {
				if dep == a {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s imports %s: package %q may only import %v", path, ip, pkg, allowed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func topPackage(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) < 2 {
		return "" // file at repo root
	}
	return parts[0]
}

func TestDependencyBudget(t *testing.T) {
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	mf, err := modfile.Parse("go.mod", b, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range mf.Require {
		if r.Indirect {
			continue
		}
		ok := false
		for _, a := range allowedExternal {
			if strings.HasPrefix(r.Mod.Path, a) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("direct dependency %s is outside the budget %v", r.Mod.Path, allowedExternal)
		}
	}
}
