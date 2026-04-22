package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/youtube/v3"
)

// Shared credential files, consistent with sibling projects.
const (
	clientSecretFile = "../client_secret.json"
	tokenFile        = "../token.json"
)

// sceneFile is the authoritative runtime state that OBS polls via
// GET /get-current-scene. It lives next to the binary and is created
// on first run with the default "LiveScene" value.
const sceneFile = "scene.json"

// The only two values ever written to scene.json. Keep these strings in
// sync with whatever OBS-side automation (lua/python/browser source)
// reads from /get-current-scene and switches scenes accordingly.
const (
	sceneLive      = "LiveScene"
	sceneSacrament = "PassingSacramentScene"
)

// listenAddr binds to loopback only so the control panel isn't
// accidentally exposed to the LAN. Change to ":44330" if access from
// other devices on the same network is desired.
const listenAddr = "127.0.0.1:44330"

type sceneState struct {
	Scene string `json:"scene"`
}

// sceneMu serializes reads/writes of scene.json so a concurrent OBS poll
// never sees a partially-written file during a scene switch.
var sceneMu sync.RWMutex

// ytSvc is used to discover the currently-active broadcast for embedding
// on the index page. Nil-safe: if the service fails to initialize (no
// token, auth error, etc.) the page still renders with a placeholder.
var ytSvc *youtube.Service

var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

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

func getClient(config *oauth2.Config) *http.Client {
	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		log.Fatalf("Unable to read %s: %v\nRun `go run main.go` in ../auth_setup to create it.", tokenFile, err)
	}
	return config.Client(context.Background(), tok)
}

// ensureSceneFile creates scene.json with the default Live scene on
// first run so OBS's first poll returns a valid payload.
func ensureSceneFile() {
	if _, err := os.Stat(sceneFile); err == nil {
		return
	}
	if err := writeScene(sceneLive); err != nil {
		log.Fatalf("Unable to create %s: %v", sceneFile, err)
	}
}

// readScene returns the current scene value stored in scene.json.
func readScene() (sceneState, error) {
	sceneMu.RLock()
	defer sceneMu.RUnlock()
	data, err := os.ReadFile(sceneFile)
	if err != nil {
		return sceneState{}, err
	}
	var s sceneState
	if err := json.Unmarshal(data, &s); err != nil {
		return sceneState{}, err
	}
	return s, nil
}

// writeScene atomically replaces scene.json with {"scene": name} by
// writing to a sibling temp file and renaming; rename is atomic on
// POSIX so a concurrent OBS poll can never observe a half-written file.
func writeScene(name string) error {
	sceneMu.Lock()
	defer sceneMu.Unlock()
	out, err := json.MarshalIndent(sceneState{Scene: name}, "", "  ")
	if err != nil {
		return err
	}
	tmp := sceneFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, sceneFile)
}

// findActiveBroadcastID returns the ID of the currently-live broadcast
// on the authenticated channel, or "" if nothing is live right now.
// The broadcast ID equals the YouTube video ID and is suitable for the
// /embed/<id> URL on the index page.
func findActiveBroadcastID() string {
	if ytSvc == nil {
		return ""
	}
	resp, err := ytSvc.LiveBroadcasts.List([]string{"id"}).
		BroadcastStatus("active").
		BroadcastType("all").
		MaxResults(5).Do()
	if err != nil {
		log.Printf("lookup active broadcast: %v", err)
		return ""
	}
	if len(resp.Items) == 0 {
		return ""
	}
	return resp.Items[0].Id
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	// http.HandleFunc("/", ...) catches every unmatched path; 404 them
	// so mistyped URLs don't silently render the control panel.
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	videoID := findActiveBroadcastID()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, struct{ VideoID string }{VideoID: videoID}); err != nil {
		log.Printf("render index: %v", err)
	}
}

func handleGetCurrentScene(w http.ResponseWriter, r *http.Request) {
	// CORS-permissive so OBS browser sources (which often send a null
	// origin) can poll the endpoint without extra configuration.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s, err := readScene()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// no-store so OBS always sees the freshest value instead of a cached
	// response that would delay the scene switch.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s)
}

func handleSetScene(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body sceneState
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if body.Scene != sceneLive && body.Scene != sceneSacrament {
		http.Error(w, "invalid scene value", http.StatusBadRequest)
		return
	}
	if err := writeScene(body.Scene); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("scene set to %s", body.Scene)
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	ctx := context.Background()

	secret, err := os.ReadFile(clientSecretFile)
	if err != nil {
		log.Fatalf("Error reading %s: %v", clientSecretFile, err)
	}
	// Readonly is enough: this project only *reads* which broadcast is
	// active. The stored token already covers this because auth_setup
	// requests the broader YoutubeScope.
	config, err := google.ConfigFromJSON(secret, youtube.YoutubeReadonlyScope)
	if err != nil {
		log.Fatalf("Unable to parse client secret file: %v", err)
	}
	client := getClient(config)

	ytSvc, err = youtube.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		// Intentionally non-fatal: the scene-switching control plane is
		// still usable even if we can't discover an active broadcast.
		log.Printf("warning: YouTube service unavailable: %v", err)
	}

	ensureSceneFile()
	abs, _ := filepath.Abs(sceneFile)
	log.Printf("scene state file: %s", abs)

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/get-current-scene", handleGetCurrentScene)
	http.HandleFunc("/set-scene", handleSetScene)

	fmt.Printf("Scene control panel: http://%s/\n", listenAddr)
	fmt.Printf("OBS polling endpoint: http://%s/get-current-scene\n", listenAddr)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// indexHTML is kept inline (rather than using `embed`) so the project
// remains a single .go file like the rest of the siblings. The template
// is rendered via html/template, which context-escapes .VideoID safely.
const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <title>Scene Control</title>
    <style>
        :root { color-scheme: dark; }
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
            background: #0d0f12; color: #eceff4; margin: 0; padding: 1.5rem;
        }
        .container { max-width: 960px; margin: 0 auto; }
        h1 { text-align: center; font-weight: 500; letter-spacing: .02em; margin: 0 0 1.25rem; }
        .video-wrapper {
            position: relative; padding-bottom: 56.25%; height: 0;
            background: #000; border-radius: 12px; overflow: hidden;
            box-shadow: 0 10px 30px rgba(0,0,0,.4);
        }
        .video-wrapper iframe, .video-wrapper .placeholder {
            position: absolute; top: 0; left: 0; width: 100%; height: 100%;
        }
        .video-wrapper iframe { border: 0; }
        .placeholder {
            display: flex; align-items: center; justify-content: center;
            color: #6b7280; font-size: 1.05rem; text-align: center; padding: 2rem;
        }
        .buttons { display: flex; gap: 1rem; margin-top: 1.5rem; justify-content: center; flex-wrap: wrap; }
        button {
            flex: 1; min-width: 200px; max-width: 320px;
            padding: 1.1rem 1.5rem; font-size: 1.05rem; font-weight: 600;
            letter-spacing: .02em; border: 0; border-radius: 10px;
            cursor: pointer; color: white;
            transition: transform .08s, filter .12s, box-shadow .12s;
        }
        button:hover { filter: brightness(1.08); }
        button:active { transform: scale(0.98); }
        button.live { background: #c62828; }
        button.sacrament { background: #283593; }
        button.active { box-shadow: 0 0 0 3px #fff, 0 0 24px rgba(255,255,255,.25); }
        .status { text-align: center; margin-top: 1.25rem; color: #9ca3af; font-size: .95rem; }
        .status b { color: #eceff4; font-weight: 600; }
        .dot {
            display: inline-block; width: 10px; height: 10px; border-radius: 50%;
            background: #4b5563; margin-right: .45rem; vertical-align: middle;
            transition: background .2s, box-shadow .2s;
        }
        .dot.live { background: #ef5350; box-shadow: 0 0 8px #ef5350; }
        .dot.sacrament { background: #5c6bc0; box-shadow: 0 0 8px #5c6bc0; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Scene Control</h1>
        <div class="video-wrapper">
            {{if .VideoID}}
            <iframe src="https://www.youtube.com/embed/{{.VideoID}}?autoplay=1&mute=1"
                    allow="autoplay; encrypted-media" allowfullscreen></iframe>
            {{else}}
            <div class="placeholder">No active live broadcast detected on the channel.</div>
            {{end}}
        </div>
        <div class="buttons">
            <button id="btn-live" class="live" onclick="setScene('LiveScene')">Live Video</button>
            <button id="btn-sacrament" class="sacrament" onclick="setScene('PassingSacramentScene')">Passing Sacrament</button>
        </div>
        <div class="status"><span class="dot" id="dot"></span>Current scene: <b id="current">&mdash;</b></div>
    </div>
    <script>
        async function setScene(name) {
            try {
                const res = await fetch('/set-scene', {
                    method: 'POST',
                    headers: {'Content-Type': 'application/json'},
                    body: JSON.stringify({scene: name})
                });
                if (!res.ok) console.error('set-scene failed', res.status, await res.text());
            } catch (e) { console.error(e); }
            refreshStatus();
        }
        function render(scene) {
            document.getElementById('current').textContent = scene || '\u2014';
            const liveActive = scene === 'LiveScene';
            const sacActive  = scene === 'PassingSacramentScene';
            document.getElementById('btn-live').classList.toggle('active', liveActive);
            document.getElementById('btn-sacrament').classList.toggle('active', sacActive);
            const dot = document.getElementById('dot');
            dot.classList.toggle('live', liveActive);
            dot.classList.toggle('sacrament', sacActive);
        }
        async function refreshStatus() {
            try {
                const res = await fetch('/get-current-scene', { cache: 'no-store' });
                if (!res.ok) return;
                const data = await res.json();
                render(data.scene);
            } catch (e) {}
        }
        refreshStatus();
        setInterval(refreshStatus, 2000);
    </script>
</body>
</html>
`
