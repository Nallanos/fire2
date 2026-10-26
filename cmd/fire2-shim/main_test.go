package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The shim is the process that must never crash, and it runs one per VM: it may
// only link the standard library, the shim package, the logging package and
// x/sys (see internal/packages/shim/CLAUDE.md). Importing internal/app for its
// logger once pulled River, pgx, chi, gRPC and the whole orchestrator into the
// binary, and ran their init() in every shim.
func TestShimLinksOnlyWhatItMust(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	allowed := func(pkg string) bool {
		switch pkg {
		case "github/nallanos/fire2/cmd/fire2-shim", // this package
			"github/nallanos/fire2/internal/packages/shim",
			"github/nallanos/fire2/internal/logging":
			return true
		}
		return strings.HasPrefix(pkg, "golang.org/x/sys/")
	}
	var unexpected []string
	for _, pkg := range strings.Fields(string(out)) {
		if !allowed(pkg) {
			unexpected = append(unexpected, pkg)
		}
	}
	if len(unexpected) > 0 {
		t.Fatalf("fire2-shim links packages it must not (%d):\n  %s", len(unexpected), strings.Join(unexpected, "\n  "))
	}
}
