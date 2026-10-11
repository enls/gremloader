// Command gremloader installs and updates software packages. See the repository README.
package main

import (
	"os"

	"github.com/enls/gremloader/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.IO{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
}
