package main

import (
	"agentbox/internal/buildinfo"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"agentbox/internal/config"
	"agentbox/internal/server"
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
	lock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		log.Fatalf("lock: %v", err)
	}
	defer lock.Close()

	srv, err := server.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	if err := srv.Run(); err != nil {
		log.Fatalf("run: %v", err)
	}
}

// lockDataDir takes an exclusive advisory lock on data_dir. Two agentbox
// processes sharing a data directory corrupt each other's state, and the
// second one only discovers the conflict when it fails to bind the listen
// port — by which time systemd has already been crash-looping the unit. The
// lock turns that into one clear error at startup instead.
//
// The returned file must stay open for the process lifetime. The kernel drops
// a flock when the holding process dies, so a crash never leaves a stale lock.
func lockDataDir(dir string) (*os.File, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "agentbox.lock"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		owner, _ := io.ReadAll(f)
		f.Close()
		log.Printf("data dir %s is locked by another process", dir)
		// Best effort only: the pid is whatever the last successful holder
		// wrote, so it can be stale if something other than agentbox took the
		// lock. Enough to start looking, not something to trust blindly.
		if held := strings.TrimSpace(string(owner)); held != "" {
			log.Printf("last recorded owner: %s (verify with: fuser -v %s/agentbox.lock)", held, dir)
		}
		log.Printf("if you meant to restart the service, use: systemctl restart agentbox")
		return nil, err
	}
	// Record the owner so the next would-be starter gets a pid to look at.
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteString("pid " + strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
