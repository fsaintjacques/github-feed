package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/fsaintjacques/github-feed/pkg/lib"
)

func main() {
	var err error

	ctx := context.Background()

	feed, events_chan, err := lib.NewEventFeed(ctx)
	if err != nil {
		log.Panic(err)
	}

	go feed.Serve()

	for events := range events_chan {
		for _, ev := range events {
			if *ev.Actor.Login == "dependabot[bot]" {
				continue
			}

			b, _ := json.Marshal(ev)
			os.Stdout.Write(b)
			os.Stdout.WriteString("\n")
		}
	}
}
