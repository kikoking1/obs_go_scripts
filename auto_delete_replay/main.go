package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

// Shared token file. Every sibling project reads the same ../token.json,
// which is produced once by running ../auth_setup. That tool requests the
// union of scopes any sibling needs, so this project's narrower scopes are
// just a subset of what the stored token already covers.
const tokenFile = "../token.json"

// getClient loads the shared token and returns an authenticated client.
// If the token is missing, it directs the user to run ../auth_setup first.
func getClient(config *oauth2.Config) *http.Client {
	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		log.Fatalf("Unable to read %s: %v\nRun `go run main.go` in ../auth_setup to create it.", tokenFile, err)
	}
	return config.Client(context.Background(), tok)
}

// Retrieves a token from a local file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// yesterdayWindow returns [start, end) covering "yesterday" in the machine's
// local timezone. Running Monday at 10:30 AM local time yields Sunday 00:00
// (inclusive) to Monday 00:00 (exclusive).
func yesterdayWindow(now time.Time) (start, end time.Time) {
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start = startOfToday.AddDate(0, 0, -1)
	end = startOfToday
	return start, end
}

// fetchYesterdaysCompletedBroadcasts pages through every completed live
// broadcast on the authenticated account and returns the ones whose scheduled
// start time falls within yesterday's local window.
func fetchYesterdaysCompletedBroadcasts(service *youtube.Service, start, end time.Time) []*youtube.LiveBroadcast {
	var matches []*youtube.LiveBroadcast
	pageToken := ""
	for {
		call := service.LiveBroadcasts.List([]string{"id", "snippet"}).
			BroadcastStatus("completed").
			BroadcastType("all").
			MaxResults(50)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		resp, err := call.Do()
		if err != nil {
			log.Fatalf("Error fetching completed broadcasts: %v", err)
		}

		for _, item := range resp.Items {
			if item.Snippet == nil || item.Snippet.ScheduledStartTime == "" {
				log.Printf("Skipping %s: no scheduled start time", item.Id)
				continue
			}
			t, err := time.Parse(time.RFC3339, item.Snippet.ScheduledStartTime)
			if err != nil {
				log.Printf("Skipping %s: cannot parse scheduled time %q: %v", item.Id, item.Snippet.ScheduledStartTime, err)
				continue
			}
			// [start, end): inclusive of start, exclusive of end.
			if !t.Before(start) && t.Before(end) {
				matches = append(matches, item)
			}
		}

		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return matches
}

func main() {
	ctx := context.Background()
	secret, err := os.ReadFile("../client_secret.json")
	if err != nil {
		log.Fatalf("Error reading client_secret.json: %v", err)
	}
	// The `youtube` scope covers both listing broadcasts and deleting videos.
	config, err := google.ConfigFromJSON(secret, youtube.YoutubeScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file: %v", err)
	}
	client := getClient(config)

	ytService, err := youtube.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Error creating YouTube service: %v", err)
	}

	start, end := yesterdayWindow(time.Now())
	fmt.Printf("Looking for completed live broadcasts scheduled between %s and %s (local).\n",
		start.Format("2006-01-02 15:04 MST"), end.Format("2006-01-02 15:04 MST"))

	broadcasts := fetchYesterdaysCompletedBroadcasts(ytService, start, end)
	if len(broadcasts) == 0 {
		fmt.Println("No completed live broadcasts from yesterday were found. Nothing to delete.")
		return
	}

	fmt.Printf("Found %d matching broadcast(s). Deleting:\n", len(broadcasts))
	var deleted, failed int
	for _, b := range broadcasts {
		title := ""
		scheduled := ""
		if b.Snippet != nil {
			title = b.Snippet.Title
			scheduled = b.Snippet.ScheduledStartTime
		}
		fmt.Printf("  %s  %q  scheduled=%s\n", b.Id, title, scheduled)

		// The broadcast ID is the same as the video ID. Deleting the video
		// removes the replay recording from the channel.
		if err := ytService.Videos.Delete(b.Id).Do(); err != nil {
			log.Printf("  FAIL  %s: %v", b.Id, err)
			failed++
			continue
		}
		fmt.Printf("  OK    %s deleted\n", b.Id)
		deleted++
	}
	fmt.Printf("Done. Deleted %d, failed %d.\n", deleted, failed)
}
