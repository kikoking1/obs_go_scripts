package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
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
	id          string
	link        string
}

// fetchOldestUpcomingStream pages through every upcoming broadcast on the
// authenticated account and returns the one with the earliest scheduled start
// time, or nil if no upcoming broadcasts exist.
func fetchOldestUpcomingStream(service *youtube.Service) *upcomingStream {
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
				id:          item.Id,
				link:        fmt.Sprintf("https://youtu.be/%s", item.Id),
			})
		}

		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	if len(streams) == 0 {
		return nil
	}
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].scheduledAt.Before(streams[j].scheduledAt)
	})
	return &streams[0]
}

// fetchRecipientEmails reads column A of the spreadsheet's first sheet starting
// at row 2 (skipping the header) and returns the trimmed, non-empty values.
func fetchRecipientEmails(service *sheets.Service, spreadsheetID string) []string {
	resp, err := service.Spreadsheets.Values.Get(spreadsheetID, "A2:A").Do()
	if err != nil {
		log.Fatalf("Error reading emails from sheet %s: %v", spreadsheetID, err)
	}

	var emails []string
	for _, row := range resp.Values {
		if len(row) == 0 {
			continue
		}
		s, ok := row[0].(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		emails = append(emails, s)
	}
	return emails
}

// sendEmail sends a plain-text message via the Gmail API from the authenticated
// user's account ("me") to a single recipient.
func sendEmail(service *gmail.Service, to, subject, body string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)

	msg := &gmail.Message{
		Raw: base64.URLEncoding.EncodeToString([]byte(b.String())),
	}
	_, err := service.Users.Messages.Send("me", msg).Do()
	return err
}

func main() {
	if err := godotenv.Load("../.env"); err != nil {
		log.Fatalf("Error loading .env file: %v", err)
	}
	SheetID = os.Getenv("EMAIL_SHEET_ID")
	if SheetID == "" {
		log.Fatal("EMAIL_SHEET_ID is not set in the .env file.")
	}

	ctx := context.Background()
	b, err := os.ReadFile("../client_secret.json")
	if err != nil {
		log.Fatalf("Error reading client_secret.json: %v", err)
	}
	config, err := google.ConfigFromJSON(b,
		youtube.YoutubeReadonlyScope,
		sheets.SpreadsheetsReadonlyScope,
		gmail.GmailSendScope,
	)
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
	gmailService, err := gmail.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		log.Fatalf("Error creating Gmail service: %v", err)
	}

	stream := fetchOldestUpcomingStream(ytService)
	if stream == nil {
		log.Fatal("No upcoming scheduled streams found on the authenticated account.")
	}
	fmt.Printf("Oldest upcoming stream: %s  %s\n",
		stream.scheduledAt.Local().Format("2006-01-02 15:04 MST"), stream.link)

	emails := fetchRecipientEmails(sheetsService, SheetID)
	if len(emails) == 0 {
		log.Fatalf("No recipient emails found in column A of sheet %s (starting at row 2).", SheetID)
	}
	fmt.Printf("Found %d recipient(s) in sheet %s.\n", len(emails), SheetID)

	subject := fmt.Sprintf("%s Sacrament Link",
		stream.scheduledAt.Local().Format("Jan 02, 2006"))
	body := stream.link

	var sent, failed int
	for _, to := range emails {
		if err := sendEmail(gmailService, to, subject, body); err != nil {
			log.Printf("  FAIL  %s: %v", to, err)
			failed++
			continue
		}
		fmt.Printf("  SENT  %s\n", to)
		sent++
	}
	fmt.Printf("Done. Sent %d, failed %d.\n", sent, failed)
}
