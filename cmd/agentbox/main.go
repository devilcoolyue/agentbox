package main

import (
	"flag"
	"log"

	"agentbox/internal/config"
	"agentbox/internal/server"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config JSON file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	log.Fatal(srv.Run())
}
