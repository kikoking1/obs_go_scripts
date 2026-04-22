package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/sheets/v4"
	"google.golang.org/api/youtube/v3"
)

// One-time authorization helper for the sibling projects.
//
// Running this tool walks through the Google OAuth consent flow once, asking
// for the union of every scope any sibling tool needs, and writes the result
// to ../token.json. From then on, every sibling project simply reads that
// shared token (alongside the shared ../client_secret.json) instead of
// maintaining its own token_*.json file.
const (
	clientSecretFile = "../client_secret.json"
	tokenFile        = "../token.json"
)

// scopes is the union of every scope any sibling tool needs. Broader scopes
// supersede narrower ones (e.g. YoutubeScope covers YoutubeReadonlyScope), so
// three entries cover all sibling projects:
//
//	auto_delete_replay        -> youtube
//	auto_email_sheets         -> youtube.readonly + sheets.readonly + gmail.send
//	auto_enable_auto_startstop-> youtube
//	auto_schedule_streams     -> youtube
//	auto_sync_links_sheets    -> youtube.readonly + sheets
var scopes = []string{
	youtube.YoutubeScope,
	sheets.SpreadsheetsScope,
	gmail.GmailSendScope,
}

func main() {
	ctx := context.Background()

	secret, err := os.ReadFile(clientSecretFile)
	if err != nil {
		log.Fatalf("Error reading %s: %v", clientSecretFile, err)
	}
	config, err := google.ConfigFromJSON(secret, scopes...)
	if err != nil {
		log.Fatalf("Unable to parse client secret file: %v", err)
	}

	if _, err := os.Stat(tokenFile); err == nil {
		fmt.Printf("An existing %s was found. Re-authorising will overwrite it.\n", tokenFile)
		fmt.Print("Press Enter to continue, or Ctrl+C to abort... ")
		var dummy string
		_, _ = fmt.Scanln(&dummy)
	}

	fmt.Println("Requesting the following scopes:")
	for _, s := range scopes {
		fmt.Printf("  - %s\n", s)
	}
	fmt.Println()

	// prompt=consent forces Google to return a refresh_token every time, even
	// when the user has previously authorised the app with a subset of scopes.
	authURL := config.AuthCodeURL(
		"state-token",
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
	fmt.Printf("Open this URL in your browser, authorise the app, then paste the resulting code here:\n\n%s\n\n", authURL)

	fmt.Print("Authorization code: ")
	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("Unable to read authorization code: %v", err)
	}

	tok, err := config.Exchange(ctx, authCode)
	if err != nil {
		log.Fatalf("Unable to exchange code for token: %v", err)
	}
	if tok.RefreshToken == "" {
		log.Println("Warning: the response did not include a refresh_token. " +
			"Sibling tools will stop working once the access token expires. " +
			"Revoke the app at https://myaccount.google.com/permissions and re-run this tool.")
	}

	saveToken(tokenFile, tok)
	fmt.Printf("\nSaved token to %s\n", tokenFile)
	fmt.Println("All sibling projects will now read this token.")
}

func saveToken(path string, token *oauth2.Token) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	if err := json.NewEncoder(f).Encode(token); err != nil {
		log.Fatalf("Unable to encode token: %v", err)
	}
}
