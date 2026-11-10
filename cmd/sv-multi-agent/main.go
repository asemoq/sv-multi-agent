package main

import (
	"log"

	"github.com/asemoq/sv-multi-agent/internal/app"
	"github.com/asemoq/sv-multi-agent/internal/config"
)

func main() {
	if err := app.Serve(config.Load()); err != nil {
		log.Fatalf("sv-multi-agent: %v", err)
	}
}
