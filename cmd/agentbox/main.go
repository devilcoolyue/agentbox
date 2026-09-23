package main

import (
	"agentbox/internal/buildinfo"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"agentbox/internal/app"
	"agentbox/internal/config"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Println(buildinfo.String("agentbox"))
		return
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup", "backup-verify", "restore":
			if err := maintenance(os.Args[1:]); err != nil {
				log.Fatal(err)
			}
			return
		}
	}
	cfgPath := flag.String("config", "config.json", "path to config JSON file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := app.Run(ctx, cfg); err != nil {
		log.Fatalf("run: %v", err)
	}
}
