// Command quai is the single Quai binary: server, worker, lint and the other sub-commands.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "quai:", err)
		os.Exit(1)
	}
}
