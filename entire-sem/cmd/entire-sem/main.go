// entire-sem is an Entire CLI external command.
//
// Once built as an executable named `entire-sem`, the parent Entire CLI
// dispatches it when a user runs `entire sem`.
package main

import (
	"fmt"
	"os"

	"github.com/entireio/cli/entire-sem/internal/cli"
)

var version = "dev"

func main() {
	if err := cli.Execute(version, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
