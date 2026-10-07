// Command jms is the jms-client command-line entry point.
package main

import (
	"os"

	"github.com/MiFaZhan/jms-client/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
