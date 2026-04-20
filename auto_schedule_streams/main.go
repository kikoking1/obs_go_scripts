package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

// MyStreamID is populated from the STREAM_ID variable in the .env file.
var MyStreamID string

const ThumbnailPath = "maxresdefault.jpg"

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

// verifyStreamID confirms the configured STREAM_ID matches a live stream on
// the authenticated account. If not, it prints the available streams so the
// user can copy the correct resource ID into .env.
func verifyStreamID(service *youtube.Service, streamID string) {
	resp, err := service.LiveStreams.List([]string{"id", "snippet"}).Mine(true).MaxResults(50).Do()
	if err != nil {
		log.Fatalf("Error listing live streams: %v", err)
	}

	for _, s := range resp.Items {
		if s.Id == streamID {
			fmt.Printf("Using stream %q (ID: %s)\n", s.Snippet.Title, s.Id)
			return
		}
	}

	fmt.Printf("STREAM_ID %q was not found on your YouTube account.\n", streamID)
	if len(resp.Items) == 0 {
		fmt.Println("No live streams exist on this account yet. Create one in YouTube Studio > Go Live > Stream.")
	} else {
		fmt.Println("Available streams (copy the ID on the left into .env as STREAM_ID):")
		for _, s := range resp.Items {
			fmt.Printf("  %s  %s\n", s.Id, s.Snippet.Title)
		}
	}
	os.Exit(1)
}

func main() {
	if err := godotenv.Load("../.env"); err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}
	MyStreamID = os.Getenv("STREAM_ID")
	if MyStreamID == "" {
		log.Fatal("STREAM_ID is not set in the .env file.")
	}

	// 1. Get the number of streams to create from command line arguments
	if len(os.Args) < 2 {
		log.Fatal("Usage: go run batch_scheduler.go [number_of_streams]")
	}
	count, err := strconv.Atoi(os.Args[1])
	if err != nil || count <= 0 {
		log.Fatal("Please provide a valid positive number of streams to create.")
	}

	ctx := context.Background()
	b, _ := os.ReadFile("../client_secret.json")
	config, _ := google.ConfigFromJSON(b, youtube.YoutubeScope)
	client := getClient(config) // Uses the getClient function from previous scripts

	service, err := youtube.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Error creating YouTube service: %v", err)
	}

	verifyStreamID(service, MyStreamID)

	thumbBytes, err := os.ReadFile(ThumbnailPath)
	if err != nil {
		log.Fatalf("Error reading thumbnail %q: %v", ThumbnailPath, err)
	}

	// 2. Find the furthest scheduled stream
	var lastStreamTime time.Time
	listCall := service.LiveBroadcasts.List([]string{"snippet"}).BroadcastStatus("upcoming").BroadcastType("all").MaxResults(50)
	resp, err := listCall.Do()
	if err != nil {
		log.Fatalf("Error fetching upcoming streams: %v", err)
	}

	if len(resp.Items) == 0 {
		fmt.Println("No upcoming streams found. Starting from the upcoming Sunday at 10:00 AM.")
		// Default logic: Find next Sunday
		now := time.Now()
		daysUntilSunday := (7 - int(now.Weekday())) % 7
		if daysUntilSunday == 0 {
			daysUntilSunday = 7
		}
		lastStreamTime = time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, now.Location()).AddDate(0, 0, daysUntilSunday)
	} else {
		for _, item := range resp.Items {
			t, _ := time.Parse(time.RFC3339, item.Snippet.ScheduledStartTime)
			if t.After(lastStreamTime) {
				lastStreamTime = t
			}
		}
		fmt.Printf("Found furthest stream at: %v\n", lastStreamTime)
	}

	// 3. Create N new streams
	for i := 1; i <= count; i++ {
		newTime := lastStreamTime.AddDate(0, 0, 7*i)

		// Note: The script uses your exact requested string for EVERY stream.
		// If you want the date in the title to update automatically, replace the Title line with:
		// Title: fmt.Sprintf("%s Coaldale Sacrament Live Stream", newTime.Format("Jan 02, 2006")),
		title := fmt.Sprintf("Sacrament Service For %s", newTime.Format("Jan 02, 2006"))
		description := fmt.Sprintf("Sacrament Service For %s", newTime.Format("Jan 02, 2006"))

		broadcast := &youtube.LiveBroadcast{
			Snippet: &youtube.LiveBroadcastSnippet{
				Title:              title,
				Description:        description,
				ScheduledStartTime: newTime.Format(time.RFC3339),
			},
			Status: &youtube.LiveBroadcastStatus{
				PrivacyStatus:           "unlisted",
				SelfDeclaredMadeForKids: false,
				// Force-send because omitempty would otherwise drop the false
				// value, leaving YouTube to prompt the creator to declare.
				ForceSendFields: []string{"SelfDeclaredMadeForKids"},
			},
			ContentDetails: &youtube.LiveBroadcastContentDetails{
				EnableAutoStart: false,
				EnableAutoStop:  false,
			},
			// Note: the YouTube Data API v3 does NOT expose a property to
			// disable live chat on a broadcast. Chat must be turned off
			// manually in YouTube Studio (Customization > Live Chat).
		}

		// Insert Broadcast
		inserted, err := service.LiveBroadcasts.Insert([]string{"snippet", "status", "contentDetails"}, broadcast).Do()
		if err != nil {
			log.Printf("Error creating stream %d: %v", i, err)
			continue
		}

		// Upload the thumbnail (this also becomes the stream's image everywhere it's shown).
		// Done before bind so a bind failure doesn't leave the broadcast without its artwork.
		if _, err := service.Thumbnails.Set(inserted.Id).Media(bytes.NewReader(thumbBytes)).Do(); err != nil {
			log.Printf("Error setting thumbnail for stream %d (%s): %v", i, inserted.Id, err)
		}

		// Bind to the stream key configured in .env.
		if _, err := service.LiveBroadcasts.Bind(inserted.Id, []string{"id"}).StreamId(MyStreamID).Do(); err != nil {
			log.Printf("Error binding stream %d: %v", i, err)
			continue
		}

		shareLink := fmt.Sprintf("https://youtu.be/%s", inserted.Id)
		fmt.Printf("Created [%d/%d]: %s | Date: %s | Link: %s\n", i, count, inserted.Id, newTime.Format("2006-01-02"), shareLink)
	}
}
