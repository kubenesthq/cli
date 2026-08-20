package sshx

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The structural half of "keys never leave the machine": the control-plane
// client and the SSH transport must not know about each other, so no code
// path can hand key material to an HTTP request. If either side ever imports
// the other, this fails the build gate rather than a design review.
func TestAPIAndSSHTransportStayIndependent(t *testing.T) {
	imports := func(dir string) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		fset := token.NewFileSet()
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", e.Name(), err)
			}
			for _, imp := range f.Imports {
				out[strings.Trim(imp.Path.Value, `"`)] = true
			}
		}
		return out
	}

	if imports("../api")["kubenest.io/cli/pkg/sshx"] {
		t.Error("pkg/api imports pkg/sshx: the control-plane client must not be able to reach key material")
	}
	if imports(".")["kubenest.io/cli/pkg/api"] {
		t.Error("pkg/sshx imports pkg/api: the SSH transport must not be able to reach the control plane")
	}
}
