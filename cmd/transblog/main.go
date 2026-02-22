package main

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"transblog/internal/cli"
)

func main() {
	// Load .env file (ignore error if not exists)
	_ = godotenv.Load()

	if err := cli.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
