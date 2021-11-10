package lib

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/google/go-github/v32/github"
	"github.com/gregjones/httpcache"
	"golang.org/x/oauth2"
)

const (
	// Poll interval header returned in github event responses.
	xPollIntervalHeader = "X-Poll-Interval"
	defaultPollSeconds  = 60
	defaultFeedCapacity = 16
	// The following numbers are taken from github API documentation.
	// https://developer.github.com/v3/activity/events/#list-public-events
	maximumEventsPages   = 10
	maximumEventsPerPage = 30
	maximumEventsPerPoll = maximumEventsPerPage * maximumEventsPages
)

type EventFeed struct {
	client *github.Client
	ctx    context.Context
	events chan<- []*github.Event
}

type Config struct {
	AuthToken string
}

func NewEventFeed(ctx context.Context, conf *Config) (*EventFeed, <-chan []*github.Event, error) {
	var feed *EventFeed = &EventFeed{ctx: ctx}

	events := make(chan []*github.Event, defaultFeedCapacity)

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: conf.AuthToken},
	)

	tc := oauth2.NewClient(ctx, ts)
	tc.Timeout = 10 * time.Second

	tc.Transport = &httpcache.Transport{
		Transport:           tc.Transport,
		Cache:               httpcache.NewMemoryCache(),
		MarkCachedResponses: true,
	}

	feed.client = github.NewClient(tc)
	feed.events = events

	return feed, events, nil
}

func (f *EventFeed) Serve() error {
	defer close(f.events)

	for {
		events, poll_interval, err := f.poll()

		// A real error was encountered
		if err != nil {
			return err
		}

		// Publish events in the channel
		f.events <- events

		select {
		case <-time.After(poll_interval):
			log.Printf("Resuming after %d seconds.", poll_interval/time.Second)
			continue
		case <-f.ctx.Done():
			return f.ctx.Err()
		}
	}
}

// Extract the poll interval hinted by github's API response. If any failure is
// encountered, default to a safe interval. github will enforce the poll
// interval, it is not necessary to be more aggressive.
func pollIntervalFromResponse(r *http.Response) time.Duration {
	// Fallback default poll interval, values taken from github's documentation.
	default_duration := time.Duration(defaultPollSeconds) * time.Second

	poll_header := r.Header.Get(xPollIntervalHeader)
	if poll_header == "" {
		return default_duration
	}

	poll_seconds, err := strconv.Atoi(poll_header)
	if err != nil {
		return default_duration
	}

	return time.Duration(poll_seconds) * time.Second
}

func (f *EventFeed) pollIntervalOrPropagateError(r *github.Response, err error) (time.Duration, bool, error) {
	if err != nil {
		switch err.(type) {
		case *github.RateLimitError:
			// RateLimiteError aren't treated as a real error. Instead, we respect
			// the rate limit reset interval for the next poll time.
			time_left := time.Until(r.Rate.Reset.Time)
			log.Printf("Rate limit exceeded, resets in %d seconds.", time_left/time.Second)
			return time_left, true, nil
		default:
			// Otherwise, propagate the error.
			return time.Duration(-1), false, err
		}
	}

	// If no error are encountered, extract the next poll interval from the
	// response header as per documentation.
	return pollIntervalFromResponse(r.Response), false, nil
}

func isCachedResponse(r *http.Response) bool {
	_, ok := r.Header[httpcache.XFromCache]
	return ok
}

func (f *EventFeed) poll() (events []*github.Event, poll_interval time.Duration, err error) {
	err = nil
	poll_interval = time.Duration(-1)

	// Consume paginated events, the loop is bounded by a known page limits.
	opts := github.ListOptions{Page: 1}
	for i := 0; i < maximumEventsPages; i++ {
		log.Printf("Polling for page %d", opts.Page)

		var response *github.Response
		var batch []*github.Event
		batch, response, err = f.client.Activity.ListEvents(f.ctx, &opts)

		var throttled bool
		poll_interval, throttled, err = f.pollIntervalOrPropagateError(response, err)

		if err != nil || throttled {
			// An actual error was encountered or github asked for a throttling.
			break
		}

		if isCachedResponse(response.Response) {
			log.Print("Response is cached")
			break
		}

		events = append(events, batch...)
		opts.Page = response.NextPage

		if response.NextPage == 0 {
			// All pages were consumed.
			break
		}
	}

	return
}
