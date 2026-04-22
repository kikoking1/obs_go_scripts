package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andreykaipov/goobs"
	"github.com/andreykaipov/goobs/api/requests/sources"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// sceneFile is the authoritative runtime state. OBS watches this file
// directly (via its own file-watcher / lua script) and switches scenes
// when the contents change. It lives next to the binary and is created
// on first run with the default "LiveScene" value.
const sceneFile = "scene.txt"

// The only two values ever written to scene.txt. Keep these strings in
// sync with whatever OBS-side automation reads the file and switches
// scenes accordingly.
const (
	sceneLive      = "LiveScene"
	sceneSacrament = "PassingSacramentScene"
)

// listenAddr binds to every interface so a phone on the same LAN can
// reach the control panel. Switch to "127.0.0.1:44330" to restrict to
// the loopback interface if that's ever desired.
const listenAddr = ":44330"

// OBS screenshot cadence. At 15 fps + 75 quality JPEG each frame is
// roughly 80-150 KB for a 1280x720 program scene, which is well
// within LAN Wi-Fi headroom and light enough that OBS keeps smooth
// encoding for the live broadcast.
const (
	frameFPS     = 15
	frameWidth   = 1280
	frameHeight  = 720
	frameQuality = 75
)

// sceneState is the shape of the JSON body POSTed to /set-scene by the
// browser UI. The on-disk file itself is plain text, not JSON.
type sceneState struct {
	Scene string `json:"scene"`
}

// sceneMu serializes reads/writes of scene.txt. The atomic rename in
// writeScene also ensures OBS's file-watcher never observes a
// half-written file during a scene switch.
var sceneMu sync.RWMutex

var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

// --- scene.txt state helpers ----------------------------------------

// ensureSceneFile creates scene.txt with the default Live scene on
// first run so OBS's file-watcher has something valid to read
// immediately.
func ensureSceneFile() {
	if _, err := os.Stat(sceneFile); err == nil {
		return
	}
	if err := writeScene(sceneLive); err != nil {
		log.Fatalf("Unable to create %s: %v", sceneFile, err)
	}
}

// readScene returns the scene name stored in scene.txt with surrounding
// whitespace trimmed, so a stray trailing newline introduced by an
// editor doesn't break equality checks.
func readScene() (string, error) {
	sceneMu.RLock()
	defer sceneMu.RUnlock()
	data, err := os.ReadFile(sceneFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeScene atomically replaces scene.txt with the bare scene name by
// writing to a sibling temp file and renaming; rename is atomic on
// POSIX so OBS's file-watcher can never observe a half-written file.
func writeScene(name string) error {
	sceneMu.Lock()
	defer sceneMu.Unlock()
	tmp := sceneFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(name), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, sceneFile)
}

// --- HTTP handlers --------------------------------------------------

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	scene, _ := readScene()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, scene); err != nil {
		log.Printf("render index: %v", err)
	}
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

// --- OBS → browser frame hub ---------------------------------------

// wsClient is a single connected browser. send is buffered so a slow
// client doesn't block the frame producer; if the buffer fills, newer
// frames are dropped rather than queued indefinitely.
type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

var (
	hubMu        sync.Mutex
	clients      = map[*wsClient]struct{}{}
	producerStop context.CancelFunc // nil unless a producer is running
)

// addClient registers a new browser subscriber and starts the OBS
// frame producer if this is the first one.
func addClient(c *wsClient) {
	hubMu.Lock()
	defer hubMu.Unlock()
	clients[c] = struct{}{}
	if producerStop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		producerStop = cancel
		go produceFrames(ctx)
	}
}

// removeClient is idempotent (see the map guard) so both the read
// and write goroutines on a client can safely call it.
func removeClient(c *wsClient) {
	hubMu.Lock()
	defer hubMu.Unlock()
	if _, ok := clients[c]; !ok {
		return
	}
	delete(clients, c)
	close(c.send)
	if len(clients) == 0 && producerStop != nil {
		producerStop()
		producerStop = nil
	}
}

// broadcast fans a JPEG frame out to every subscriber. If a client's
// send buffer is full we drop this frame for them rather than stalling
// the producer — a dropped frame is always better than a stalled feed.
func broadcast(frame []byte) {
	hubMu.Lock()
	defer hubMu.Unlock()
	for c := range clients {
		select {
		case c.send <- frame:
		default:
		}
	}
}

var upgrader = websocket.Upgrader{
	// Accept any origin; this server is a LAN tool where the operator
	// reaches it via a bare IP and there is no cookie-based auth to
	// protect from cross-site requests.
	CheckOrigin: func(*http.Request) bool { return true },
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	c := &wsClient{conn: conn, send: make(chan []byte, 2)}
	addClient(c)
	log.Printf("stream client connected (%s); clients=%d", conn.RemoteAddr(), clientCount())

	// Read pump: the browser doesn't actually send anything, but we
	// need a goroutine draining reads so gorilla can detect close/ping
	// failures and so ping/pong keep-alive works.
	go func() {
		defer func() {
			removeClient(c)
			_ = conn.Close()
			log.Printf("stream client disconnected (%s); clients=%d", conn.RemoteAddr(), clientCount())
		}()
		conn.SetReadLimit(512)
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		})
		for {
			if _, _, err := conn.NextReader(); err != nil {
				return
			}
		}
	}()

	// Write pump.
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case frame, ok := <-c.send:
			if !ok {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
				return
			}
		case <-ping.C:
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func clientCount() int {
	hubMu.Lock()
	defer hubMu.Unlock()
	return len(clients)
}

// produceFrames pulls JPEG screenshots of the current program scene
// from OBS at `frameFPS` and broadcasts them to all subscribers.
// Survives OBS restarts by reconnecting with a 2s backoff.
func produceFrames(ctx context.Context) {
	host := env("OBS_WS_HOST", "127.0.0.1")
	port := env("OBS_WS_PORT", "4455")
	password := os.Getenv("OBS_WS_PASSWORD")
	addr := net.JoinHostPort(host, port)

	var obs *goobs.Client
	defer func() {
		if obs != nil {
			_ = obs.Disconnect()
		}
		log.Printf("frame producer stopped")
	}()

	ticker := time.NewTicker(time.Second / time.Duration(frameFPS))
	defer ticker.Stop()

	var currentScene string
	var sceneCheckedAt time.Time

	log.Printf("frame producer started (obs-websocket=%s, %dfps)", addr, frameFPS)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if obs == nil {
				var opts []goobs.Option
				if password != "" {
					opts = append(opts, goobs.WithPassword(password))
				}
				c, err := goobs.New(addr, opts...)
				if err != nil {
					log.Printf("obs-websocket connect %s: %v (retrying in 2s)", addr, err)
					select {
					case <-ctx.Done():
						return
					case <-time.After(2 * time.Second):
					}
					continue
				}
				obs = c
				currentScene = ""
				log.Printf("connected to obs-websocket at %s", addr)
			}

			// Refresh the program scene name periodically so we follow
			// scene switches made in OBS (by the hotkey, the mobile
			// control panel, the OBS-side lua, etc.).
			if time.Since(sceneCheckedAt) > 500*time.Millisecond {
				resp, err := obs.Scenes.GetCurrentProgramScene()
				if err != nil {
					log.Printf("get program scene: %v (reconnecting)", err)
					_ = obs.Disconnect()
					obs = nil
					continue
				}
				name := resp.SceneName
				if name == "" {
					name = resp.CurrentProgramSceneName // older obs-websocket
				}
				currentScene = name
				sceneCheckedAt = time.Now()
			}

			if currentScene == "" {
				continue
			}

			params := sources.NewGetSourceScreenshotParams().
				WithSourceName(currentScene).
				WithImageFormat("jpg").
				WithImageWidth(frameWidth).
				WithImageHeight(frameHeight).
				WithImageCompressionQuality(frameQuality)

			resp, err := obs.Sources.GetSourceScreenshot(params)
			if err != nil {
				log.Printf("get screenshot: %v (reconnecting)", err)
				_ = obs.Disconnect()
				obs = nil
				continue
			}

			raw, err := decodeDataURL(resp.ImageData)
			if err != nil {
				log.Printf("decode screenshot: %v", err)
				continue
			}
			broadcast(raw)
		}
	}
}

// decodeDataURL strips a `data:<mime>;base64,` prefix (if present)
// from obs-websocket's ImageData field and returns the raw JPEG bytes.
func decodeDataURL(s string) ([]byte, error) {
	if i := strings.Index(s, ";base64,"); i >= 0 {
		s = s[i+len(";base64,"):]
	}
	return base64.StdEncoding.DecodeString(s)
}

// --- misc helpers ---------------------------------------------------

// lanIPv4s returns every reachable private IPv4 address on this host
// (10/8, 172.16/12, 192.168/16) so we can print a usable LAN URL in
// the startup banner without the operator having to look it up.
func lanIPv4s() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() || !ip4.IsPrivate() {
				continue
			}
			s := ip4.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// port extracts just the port from listenAddr so we can build URLs.
func port() string {
	if _, p, err := net.SplitHostPort(listenAddr); err == nil {
		return p
	}
	return listenAddr
}

// --- main -----------------------------------------------------------

func main() {
	// .env is shared at the repo root (../.env) so every sibling reads
	// the same file. Errors here are non-fatal — the user may not have
	// an .env yet and OBS_WS_PASSWORD has a sensible empty default.
	if err := godotenv.Load("../.env"); err != nil {
		log.Printf("note: couldn't load ../.env (%v); reading env vars from the environment instead", err)
	}

	ensureSceneFile()
	abs, _ := filepath.Abs(sceneFile)
	log.Printf("scene state file: %s", abs)

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/set-scene", handleSetScene)
	http.HandleFunc("/stream", handleStream)

	p := port()
	fmt.Println()
	fmt.Println("Scene control panel:")
	fmt.Printf("  http://localhost:%s/\n", p)
	fmt.Printf("  http://127.0.0.1:%s/\n", p)
	for _, ip := range lanIPv4s() {
		fmt.Printf("  http://%s:%s/   (use this from your phone on the same LAN)\n", ip, p)
	}
	fmt.Println()
	fmt.Printf("OBS scene file:       %s\n", abs)
	fmt.Printf("Browser video stream: ws://localhost:%s/stream\n", p)
	fmt.Println()

	srv := &http.Server{
		Addr:         listenAddr,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // WebSocket writes set their own per-message deadlines.
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// indexHTML is kept inline so the project remains a single .go file.
// The template is rendered via html/template; there are no dynamic
// fields anymore (the video feed is pulled live over WS on the client).
const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
    <title>Scene Control</title>
    <style>
        :root { color-scheme: dark; }
        * { box-sizing: border-box; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
            background: #0d0f12; color: #eceff4; margin: 0; padding: 1.25rem;
            -webkit-tap-highlight-color: transparent;
        }
        .container { max-width: 960px; margin: 0 auto; }
        h1 { text-align: center; font-weight: 500; letter-spacing: .02em; margin: 0 0 1rem; }
        .video-wrapper {
            position: relative; padding-bottom: 56.25%; height: 0;
            background: #000; border-radius: 12px; overflow: hidden;
            box-shadow: 0 10px 30px rgba(0,0,0,.4);
        }
        canvas#video {
            position: absolute; top: 0; left: 0; width: 100%; height: 100%;
            display: block; background: #000;
        }
        .placeholder {
            position: absolute; top: 0; left: 0; width: 100%; height: 100%;
            display: flex; align-items: center; justify-content: center;
            color: #9ca3af; font-size: 1rem; text-align: center; padding: 2rem;
            background: rgba(0,0,0,.55); transition: opacity .25s;
            pointer-events: none;
        }
        .placeholder.hidden { opacity: 0; }
        .buttons { display: flex; gap: 1rem; margin-top: 1.25rem; justify-content: center; flex-wrap: wrap; }
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
        .status { text-align: center; margin-top: 1rem; color: #9ca3af; font-size: .95rem; }
        .status b { color: #eceff4; font-weight: 600; }
        .dot {
            display: inline-block; width: 10px; height: 10px; border-radius: 50%;
            background: #4b5563; margin-right: .45rem; vertical-align: middle;
            transition: background .2s, box-shadow .2s;
        }
        .dot.live { background: #ef5350; box-shadow: 0 0 8px #ef5350; }
        .dot.sacrament { background: #5c6bc0; box-shadow: 0 0 8px #5c6bc0; }
        .fps { text-align: center; margin-top: .4rem; color: #4b5563; font-size: .8rem; font-variant-numeric: tabular-nums; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Scene Control</h1>
        <div class="video-wrapper">
            <canvas id="video" width="1280" height="720"></canvas>
            <div class="placeholder" id="placeholder">Waiting for OBS stream&hellip;</div>
        </div>
        <div class="buttons">
            <button id="btn-live" class="live" onclick="setScene('LiveScene')">Live Video</button>
            <button id="btn-sacrament" class="sacrament" onclick="setScene('PassingSacramentScene')">Passing Sacrament</button>
        </div>
        <div class="status"><span class="dot" id="dot"></span>Current scene: <b id="current">{{.}}</b></div>
        <div class="fps" id="fps"></div>
    </div>
    <script>
        const canvas      = document.getElementById('video');
        const ctx         = canvas.getContext('2d');
        const placeholder = document.getElementById('placeholder');
        const fpsEl       = document.getElementById('fps');

        let lastFrameAt = 0;
        let frameTimes  = [];

        function connectStream() {
            const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
            const ws = new WebSocket(proto + '//' + location.host + '/stream');
            ws.binaryType = 'arraybuffer';
            ws.onmessage = async (ev) => {
                try {
                    const blob = new Blob([ev.data], { type: 'image/jpeg' });
                    const bitmap = await createImageBitmap(blob);
                    if (canvas.width  !== bitmap.width)  canvas.width  = bitmap.width;
                    if (canvas.height !== bitmap.height) canvas.height = bitmap.height;
                    ctx.drawImage(bitmap, 0, 0);
                    bitmap.close();
                    lastFrameAt = Date.now();
                    placeholder.classList.add('hidden');
                    frameTimes.push(lastFrameAt);
                    const cutoff = lastFrameAt - 1000;
                    while (frameTimes.length && frameTimes[0] < cutoff) frameTimes.shift();
                    fpsEl.textContent = frameTimes.length + ' fps';
                } catch (e) { console.error('frame decode:', e); }
            };
            ws.onclose = () => setTimeout(connectStream, 1500);
            ws.onerror = () => { try { ws.close(); } catch (e) {} };
        }

        setInterval(() => {
            if (Date.now() - lastFrameAt > 3000) {
                placeholder.classList.remove('hidden');
                placeholder.textContent = 'Waiting for OBS stream…';
                fpsEl.textContent = '';
            }
        }, 1000);

        connectStream();

        async function setScene(name) {
            try {
                const res = await fetch('/set-scene', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ scene: name })
                });
                if (!res.ok) {
                    console.error('set-scene failed', res.status, await res.text());
                    return;
                }
                render(name);
            } catch (e) { console.error(e); }
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
        // The server rendered the current scene into #current on page load;
        // sync the button/dot highlights to match it.
        render(document.getElementById('current').textContent.trim());
    </script>
</body>
</html>
`
