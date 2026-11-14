// Command emx manages every inference engine through one CLI and one endpoint.
package main

import (
	"os"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
