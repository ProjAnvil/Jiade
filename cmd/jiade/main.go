package main

import (
	"fmt"
	"os"

	"github.com/projanvil/jiade/internal/cli"
)

func main() {
	if err := cli.New().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "jiade:", err)
		os.Exit(1)
	}
}
