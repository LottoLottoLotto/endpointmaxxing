// Command substrate manages every inference engine through one CLI and one endpoint.
package main

import (
	"os"

	"github.com/lotto/substrate/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
