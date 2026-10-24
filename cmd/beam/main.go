package main

import (
	"context"
	"os"

	"github.com/Beam-Network/beam-cli-public/internal/command"
	"github.com/Beam-Network/beam-cli-public/internal/version"
)

func main() {
	app := command.New(version.Info())
	os.Exit(app.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
