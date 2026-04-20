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

	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/youtube/v3"
)

// SheetID is populated from the SHEET_ID variable in the .env file.
var SheetID string

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

type upcomingStream struct {
	scheduledAt time.Time
	link        string
}

// fetchUpcomingStreams pages through every upcoming broadcast on the
// authenticated account and returns them sorted by scheduled start time.
func fetchUpcomingStreams(service *youtube.Service) []upcomingStream {
	var streams []upcomingStream
	pageToken := ""
	for {
		call := service.LiveBroadcasts.List([]string{"snippet"}).
			BroadcastStatus("upcoming").
			BroadcastType("all").
			MaxResults(50)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		resp, err := call.Do()
		if err != nil {
			log.Fatalf("Error fetching upcoming streams: %v", err)
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
			streams = append(streams, upcomingStream{
				scheduledAt: t,
				link:        fmt.Sprintf("https://youtu.be/%s", item.Id),
			})
		}

		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	sort.Slice(streams, func(i, j int) bool {
		return streams[i].scheduledAt.Before(streams[j].scheduledAt)
	})
	return streams
}

// writeToSheet clears columns A:B of the target spreadsheet's first sheet and
// writes a header row followed by one row per stream:
// [ScheduledDate, ShareableLink].
func writeToSheet(service *sheets.Service, spreadsheetID string, streams []upcomingStream) {
	if _, err := service.Spreadsheets.Values.Clear(spreadsheetID, "A:B", &sheets.ClearValuesRequest{}).Do(); err != nil {
		log.Fatalf("Error clearing sheet %s: %v", spreadsheetID, err)
	}

	values := [][]interface{}{
		{"ScheduledDate", "ShareableLink"},
	}
	for _, s := range streams {
		values = append(values, []interface{}{
			s.scheduledAt.Local().Format("2006-01-02"),
			s.link,
		})
	}

	_, err := service.Spreadsheets.Values.Update(spreadsheetID, "A1", &sheets.ValueRange{
		Values: values,
	}).ValueInputOption("RAW").Do()
	if err != nil {
		log.Fatalf("Error writing to sheet %s: %v", spreadsheetID, err)
	}
}

func main() {
	if err := godotenv.Load("../.env"); err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}
	SheetID = os.Getenv("SHEET_ID")
	if SheetID == "" {
		log.Fatal("SHEET_ID is not set in the .env file.")
	}

	ctx := context.Background()
	b, err := os.ReadFile("../client_secret.json")
	if err != nil {
		log.Fatalf("Error reading client_secret.json: %v", err)
	}
	config, err := google.ConfigFromJSON(b, youtube.YoutubeReadonlyScope, sheets.SpreadsheetsScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file: %v", err)
	}
	client := getClient(config)

	ytService, err := youtube.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Error creating YouTube service: %v", err)
	}

	sheetsService, err := sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Error creating Sheets service: %v", err)
	}

	streams := fetchUpcomingStreams(ytService)
	fmt.Printf("Found %d upcoming streams.\n", len(streams))
	for _, s := range streams {
		fmt.Printf("  %s  %s\n", s.scheduledAt.Local().Format("2006-01-02"), s.link)
	}

	writeToSheet(sheetsService, SheetID, streams)
	fmt.Printf("Wrote %d rows to sheet %s.\n", len(streams), SheetID)
}
