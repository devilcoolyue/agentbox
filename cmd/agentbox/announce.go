package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"agentbox/internal/announce"
	"agentbox/internal/config"
)

// announceCommand manages the site-wide announcement from the host, for
// release scripts that cannot sign in. The running server picks the change up
// within a few seconds and pushes it to open pages.
//
//	agentbox announce --config C                                  print the current one
//	agentbox announce --config C --release --in 5m --duration 2  schedule a release
//	agentbox announce --config C --notice --message "..."        post a notice
//	agentbox announce --config C --clear                         withdraw it
func announceCommand(args []string, output io.Writer, now time.Time) error {
	flags := flag.NewFlagSet("announce", flag.ContinueOnError)
	path := flags.String("config", "config.json", "configuration path")
	release := flags.Bool("release", false, "announce a release (countdown, then \"upgraded\")")
	notice := flags.Bool("notice", false, "announce a plain notice")
	clear := flags.Bool("clear", false, "withdraw the current announcement")
	in := flags.Duration("in", 0, "release starts this long from now (e.g. 5m)")
	at := flags.String("at", "", "release starts at this RFC 3339 time")
	duration := flags.Int("duration", 0, "expected interruption in minutes")
	message := flags.String("message", "", "banner text")
	from := flags.String("from-version", "", "version running now; the release completes when another version starts")
	by := flags.String("by", "release", "name shown as the poster")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected announce arguments")
	}
	actions := 0
	for _, on := range []bool{*release, *notice, *clear} {
		if on {
			actions++
		}
	}
	if actions > 1 {
		return errors.New("choose one of --release, --notice or --clear")
	}
	if *in != 0 && *at != "" {
		return errors.New("use either --in or --at")
	}
	if !*release && (*in != 0 || *at != "" || *duration != 0 || *from != "") {
		return errors.New("--in, --at, --duration and --from-version need --release")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	svc := announce.New(cfg.DataDir)
	var current *announce.Announcement
	switch {
	case *clear:
		if err = svc.Clear(); err != nil {
			return err
		}
	case *release || *notice:
		input := announce.Input{Kind: announce.KindNotice, Message: *message}
		if *release {
			input.Kind, input.DurationMin = announce.KindRelease, *duration
			switch {
			case *in < 0:
				return errors.New("--in must not be negative")
			case *in > 0:
				input.StartsAt = now.Add(*in).UnixMilli()
			case *at != "":
				t, err := time.Parse(time.RFC3339, *at)
				if err != nil {
					return fmt.Errorf("--at: %w", err)
				}
				input.StartsAt = t.UnixMilli()
			}
		}
		if current, err = svc.Put(input, *by, *from, now); err != nil {
			return err
		}
	default:
		if current, err = svc.Get(); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(map[string]any{"announcement": current}, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, string(raw))
	return err
}
