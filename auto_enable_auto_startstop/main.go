package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
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

type upcomingBroadcast struct {
	scheduledAt time.Time
	broadcast   *youtube.LiveBroadcast
}

// fetchUpcomingBroadcasts pages through every upcoming broadcast on the
// authenticated account and returns them sorted by scheduled start time
// (earliest first).
func fetchUpcomingBroadcasts(service *youtube.Service) []upcomingBroadcast {
	var items []upcomingBroadcast
	pageToken := ""
	for {
		call := service.LiveBroadcasts.List([]string{"id", "snippet", "contentDetails"}).
			BroadcastStatus("upcoming").
			BroadcastType("all").
			MaxResults(50)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		resp, err := call.Do()
		if err != nil {
			log.Fatalf("Error fetching upcoming broadcasts: %v", err)
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
			items = append(items, upcomingBroadcast{
				scheduledAt: t,
				broadcast:   item,
			})
		}

		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].scheduledAt.Before(items[j].scheduledAt)
	})
	return items
}

// enableAutoStartStop sets EnableAutoStart and EnableAutoStop to true on the
// provided broadcast and returns the updated broadcast as returned by the API.
//
// Note: liveBroadcasts.update replaces every mutable field in each `part`
// that is sent. We therefore reuse the broadcast's existing ContentDetails
// (retrieved via the list call) and only flip the two flags so nothing else
// in contentDetails is clobbered.
func enableAutoStartStop(service *youtube.Service, b *youtube.LiveBroadcast) *youtube.LiveBroadcast {
	cd := b.ContentDetails
	if cd == nil {
		cd = &youtube.LiveBroadcastContentDetails{}
	}
	cd.EnableAutoStart = true
	cd.EnableAutoStop = true
	cd.ForceSendFields = append(cd.ForceSendFields, "EnableAutoStart", "EnableAutoStop")

	// The API rejects a contentDetails update unless monitorStream is present
	// with broadcastStreamDelayMs set. The Go client's omitempty would drop
	// zero-valued ints/bools, so force-send them to keep the existing values.
	if cd.MonitorStream == nil {
		cd.MonitorStream = &youtube.MonitorStreamInfo{}
	}
	cd.MonitorStream.ForceSendFields = append(cd.MonitorStream.ForceSendFields,
		"BroadcastStreamDelayMs", "EnableMonitorStream")

	update := &youtube.LiveBroadcast{
		Id:             b.Id,
		ContentDetails: cd,
	}

	resp, err := service.LiveBroadcasts.Update([]string{"id", "contentDetails"}, update).Do()
	if err != nil {
		log.Fatalf("Error updating broadcast %s: %v", b.Id, err)
	}
	return resp
}

func main() {
	ctx := context.Background()
	secret, err := os.ReadFile("../client_secret.json")
	if err != nil {
		log.Fatalf("Error reading client_secret.json: %v", err)
	}
	config, err := google.ConfigFromJSON(secret, youtube.YoutubeScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file: %v", err)
	}
	client := getClient(config)

	ytService, err := youtube.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Error creating YouTube service: %v", err)
	}

	upcoming := fetchUpcomingBroadcasts(ytService)
	if len(upcoming) == 0 {
		fmt.Println("No upcoming scheduled streams found.")
		return
	}

	oldest := upcoming[0]
	fmt.Printf("Oldest upcoming stream: %s\n", oldest.broadcast.Id)
	fmt.Printf("  Title:      %s\n", oldest.broadcast.Snippet.Title)
	fmt.Printf("  Scheduled:  %s\n", oldest.scheduledAt.Local().Format("2006-01-02 15:04 MST"))
	fmt.Printf("  Link:       https://youtu.be/%s\n", oldest.broadcast.Id)
	if cd := oldest.broadcast.ContentDetails; cd != nil {
		fmt.Printf("  Before:     EnableAutoStart=%t  EnableAutoStop=%t\n",
			cd.EnableAutoStart, cd.EnableAutoStop)
	}

	updated := enableAutoStartStop(ytService, oldest.broadcast)
	if cd := updated.ContentDetails; cd != nil {
		fmt.Printf("  After:      EnableAutoStart=%t  EnableAutoStop=%t\n",
			cd.EnableAutoStart, cd.EnableAutoStop)
	}
	fmt.Println("Done.")
}
