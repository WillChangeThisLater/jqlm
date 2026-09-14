// gojq - Go implementation of jq
package main

import (
	"os"

	"github.com/WillChangeThisLater/jqlm/cli"
)

func main() {
	os.Exit(cli.Run())
}
