package main

import (
	"os"

	"wdp/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
