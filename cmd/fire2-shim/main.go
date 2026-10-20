// Command fire2-shim keeps one Firecracker VM alive independently of the
// worker: the worker spawns one per sandbox, and the shim becomes the VM's
// parent. See internal/packages/shim.
package main

import (
	"os"

	"github/nallanos/fire2/internal/packages/shim"
)

func main() {
	os.Exit(shim.Main(os.Args[1:]))
}
