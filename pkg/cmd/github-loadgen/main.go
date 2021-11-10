package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	feed "github.com/fsaintjacques/github-feed/pkg/lib"
	"github.com/google/go-github/v32/github"
)

var optableGithubURL = "https://staging1.cloud-dev.optable.co/my-super-site/identify?cookies=yes"

var cookies = make(map[string]http.CookieJar)
var cMu = sync.RWMutex{}

func clientFor(user string) *http.Client {
	cMu.RLocker().Lock()
	jar, found := cookies[user]
	cMu.RLocker().Unlock()

	if !found {
		jar, _ = cookiejar.New(nil)
		cMu.Lock()
		// In practice we should re-check, but here we don't really care.
		cookies[user] = jar
		cMu.Unlock()
	}

	return &http.Client{Jar: jar}
}

var privateEmailMatcher = regexp.MustCompile(`(noreply.github.com$|\.local$)`)

func matchEmail(email string) bool {
	return email != "" && !privateEmailMatcher.MatchString(email)
}

func hashEmail(email string) string {
	hashed := sha256.Sum256([]byte(email))
	return hex.EncodeToString(hashed[:])
}

func gatherIdsFromCommits(user string, event *github.Event) (ids []string) {
	ids = make([]string, 0, 1)
	ids = append(ids, "c:"+user)

	payload, err := event.ParsePayload()
	if err != nil || event.GetType() != "PushEvent" {
		return
	}

	seen := make(map[string]bool, 16)

	pushPayload := payload.(*github.PushEvent)
	for _, commit := range pushPayload.Commits {
		author := strings.ToLower(commit.GetAuthor().GetEmail())
		if !matchEmail(author) {
			continue
		}

		// Skip if we already saw this email.
		if _, found := seen[author]; found {
			continue
		}

		seen[author] = true
		ids = append(ids, "e:"+hashEmail(author))
	}

	return
}

func sendEvent(event *github.Event) {
	user := strings.ToLower(event.Actor.GetLogin())
	ids := gatherIdsFromCommits(user, event)
	if len(ids) < 1 {
		return
	}

	c := clientFor(user)

	payload, err := json.Marshal(ids)
	if err != nil {
		log.Printf("Failed marshalling ids: %v", err)
	}

	req, err := http.NewRequest("POST", optableGithubURL, bytes.NewReader(payload))
	if err != nil {
		log.Printf("Error creating request: %v", err)
	}

	req.Header.Set("User-Agent", "github-loadgen")

	rep, err := c.Do(req)
	if err != nil {
		log.Printf("Error with request: %v", err)
		return
	}

	defer rep.Body.Close()

	if rep.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(rep.Body)
		log.Printf("Error with request code (%s): %s, %s", rep.Status, body, payload)
		return
	}
}

// Github bots ends with `[?bot]?`. This will induce false positives, but we
// can tolerate it.
var botMatcher = regexp.MustCompile(`(?i)\[?bot\]?$`)

func matchEvent(e *github.Event) bool {
	return !botMatcher.Match([]byte(*e.Actor.Login))
}

func processEvent(event *github.Event) {
	if !matchEvent(event) {
		return
	}

	sendEvent(event)
}

func rateLimit(events []*github.Event) <-chan *github.Event {
	n := len(events)
	feed := make(chan *github.Event, n)

	go func() {
		tick := int(60*time.Second) / n
		ticker := time.NewTicker(time.Duration(tick))

		defer ticker.Stop()
		for _, e := range events {
			<-ticker.C
			feed <- e
		}

		close(feed)
	}()

	return feed
}

func processBatch(batch []*github.Event) {
	log.Printf("Consuming %d events", len(batch))
	for e := range rateLimit(batch) {
		go processEvent(e)
	}
}

func main() {
	ctx := context.Background()

	conf := &feed.Config{
		AuthToken: os.Getenv("GITHUB_AUTH_TOKEN"),
	}

	feed, events, err := feed.NewEventFeed(ctx, conf)
	if err != nil {
		log.Panic(err)
	}

	go func() { log.Panic(feed.Serve()) }()

	for batch := range events {
		go processBatch(batch)
	}
}
