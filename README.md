# OBS Sunday Stream Automation (Raspberry Pi)

A collection of small Go tools and shell scripts that run a weekly
church live stream (OBS → YouTube) from a Raspberry Pi with almost no
manual intervention. Cron handles the schedule; the pieces talk to each
other through a shared Google OAuth token and a couple of text files.

If you're the person inheriting this setup, you should be able to get
to the finish line by following this file top to bottom.

---

## How a week plays out

| When             | What happens                                                        |
| ---------------- | ------------------------------------------------------------------- |
| **Sat 9:00 PM**  | Email tomorrow's YouTube link to everyone in the subscribers sheet. |
| **Sat 9:00 PM**  | Schedule one more weekly broadcast to keep the queue full.          |
| **Sat 9:10 PM**  | Refresh the public "upcoming streams" Google Sheet.                 |
| **Sun 9:10 AM**  | Flip auto-start on today's scheduled broadcast.                     |
| **Sun 9:20 AM**  | Start OBS; streaming begins; YouTube auto-goes-live.                |
| **Sun 10:42 AM** | Kill OBS. YouTube's auto-stop ends the broadcast once OBS disconnects. |
| **Mon 10:42 AM** | Delete yesterday's replay from the channel.                         |

Separately, while the service is actually running, an operator can open
a web page on their phone (over Wi-Fi) to switch between the two OBS
scenes (`LiveScene` and `PassingSacramentScene`). That's the
`scene_switcher` tool.

---

## What's in the repo

| Folder                        | Binary                       | What it does                                                                                                     |
| ----------------------------- | ---------------------------- | ---------------------------------------------------------------------------------------------------------------- |
| `auth_setup/`                 | run via `go run main.go`     | One-time Google login. Produces `../token.json`; every other tool reads it.                                      |
| `auto_email_sheets/`          | `auto_emailsheets`           | Emails the next stream link to everyone in a Google Sheet.                                                       |
| `auto_schedule_streams/`      | `autoschedulestream N`       | Creates `N` new weekly scheduled broadcasts, using `maxresdefault.jpg` as the thumbnail.                         |
| `auto_sync_links_sheets/`     | `auto_syncsheets`            | Dumps every upcoming broadcast (date + link) into a shared Google Sheet.                                         |
| `auto_enable_auto_startstop/` | `auto_enable_auto_startstop` | Turns on Youtube's Enable "auto start / auto stop" for the nearest upcoming broadcast.                                            |
| `auto_delete_replay/`         | `auto_delete_replay`         | Deletes yesterday's completed broadcast so old recordings don't pile up.                                         |
| `scene_switcher/`             | `scene_switcher`             | Phone web UI to switch OBS scenes during the service. Listens on port **44330**. Writes to `scene.txt`.          |
| `start_obs.sh`                | —                            | Launches OBS and auto starts streaming to youtube (which triggers youtube to go live automatically)                                                       |
| `stop_obs.sh`                 | —                            | Ends the Sunday service. Sends SIGTERM to OBS (`killall obs`), waits 10 s, then SIGKILL (`killall -9 obs`) as a safety net. |

Shared files at the repo root:

- `client_secret.json` — Google OAuth "Desktop app" credentials (you download this from Google Cloud Console).
- `token.json` — created by `auth_setup/`.
- `.env` — per-host configuration. Copy from `.env.example`.
- `logs/` — where cron writes each tool's output.

---

## What you need

**Hardware & OS**

- A Raspberry Pi (tested on a Pi 5 running Raspberry Pi OS Bookworm 64-bit).
- Auto-login to the desktop enabled — **Preferences → Raspberry Pi Configuration → System → Auto login**. OBS needs a real graphical session at the scheduled hour, which only exists if someone is "logged in".
- An HDMI capture card or whatever video source your OBS scenes expect.

**Install these packages first**

```bash
sudo apt update
sudo apt install -y git obs-studio ufw golang-go
```

**On the Google side**

- A Google Cloud project with the **YouTube Data API v3**, **Google Sheets API**, and **Gmail API** enabled.
- An OAuth 2.0 client of type **Desktop app**. Download its JSON and save it as `client_secret.json` in the repo root.
- A YouTube channel with live streaming enabled and one persistent live stream key set up (this is what OBS streams into).
- Two Google Sheets — one for subscriber emails (column A, starting row 2), one for the public upcoming-streams schedule.

---

## One-time setup

> Every cron line below assumes the repo lives at
> `/home/kiko/cronjobs/obs_go_scripts`. If your Linux user isn't `kiko`,
> either create that path anyway or find-and-replace `/home/kiko` in the
> crontab with your own home directory.

### 1. Clone the repo

```bash
mkdir -p ~/cronjobs
cd ~/cronjobs
git clone <your-repo-url> obs_go_scripts
cd obs_go_scripts
```

### 2. Drop in your Google credentials

Put the OAuth JSON you downloaded in the repo root, renamed to `client_secret.json`:


### 3. Fill in `.env`

| Key              | How to find it                                                                                                                           |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `STREAM_ID`      | YouTube Studio → Go Live → Stream → your persistent stream key. If you guess wrong, `auto_schedule_streams` prints the valid choices.    |
| `SHEET_ID`       | The ID in the URL of your public upcoming-streams sheet (`.../spreadsheets/d/THIS_PART/edit`).                                           |
| `EMAIL_SHEET_ID` | The ID of your subscribers sheet.                                                                                                        |

Optional — only needed if you set a password on OBS's WebSocket server:

```env
OBS_WS_PASSWORD="your-obs-websocket-password"
OBS_WS_HOST=127.0.0.1
OBS_WS_PORT=4455
```

### 4. One-time Google login

```bash
cd auth_setup
go run main.go
```

Open the printed URL in a browser, approve, and paste the code back into the terminal and hit enter (this is the part in the resulting broken localhost url that you get redirected to, between the code= and &). This writes `../token.json`, which the other tools reuse. If a Google login ever expires (usually never, once a refresh token is cached), run this again — it will overwrite the old token.

### 5. Build each tool

Uneccessary if you're running on raspberry pi OS, as the built binaries are already included, but you can build the binaries for you system by doing something like the below (or manually go into each folder and do `go build`)

```bash
cd ..
for d in auto_delete_replay auto_email_sheets auto_enable_auto_startstop \
         auto_schedule_streams auto_sync_links_sheets scene_switcher; do
  echo "Building $d ..."
  (cd "$d" && go build)
done
```

Each folder now contains its own compiled binary.

### 6. Install the cron schedule

Make sure your System is runing in a timezone you'd expect (if you're on linux), by running the following: `sudo timedatectl set-timezone America/Edmonton`. You can get your timezone value by getting the closest major city from the list printed out by running this command `timedatectl list-timezones`.

Open your user's crontab:

```bash
crontab -e
```

Paste in (adjust `/home/kiko` to your own home if needed, and tweak the times to match your local timezone):

```cron
#0 20 * * SAT truncate -s 0 /home/kiko/cronjobs/obs_go_scripts/logs/*.log
#0 21 * * SAT cd /home/kiko/cronjobs/obs_go_scripts/auto_email_sheets && ./auto_emailsheets >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_emailsheets.log 2>&1
#0 21 * * SAT cd /home/kiko/cronjobs/obs_go_scripts/auto_schedule_streams && ./autoschedulestream 1 >> /home/kiko/cronjobs/obs_go_scripts/logs/autoschedulestream.log 2>&1
#10 21 * * SAT cd /home/kiko/cronjobs/obs_go_scripts/auto_sync_links_sheets && ./auto_syncsheets >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_syncsheets.log 2>&1
#0 22 * * SAT sudo /usr/bin/bash /home/kiko/cronjobs/obs_go_scripts/pi_sleep_wake_till_sunday.sh
##wakes up on Sunday @ 8:30am
#*/5 * * * SUN cd /home/kiko/cronjobs/obs_go_scripts/scene_switcher/ && ./scene_switcher >> /home/kiko/cronjobs/obs_go_scripts/logs/scene_switcher.log 2>&1
#10 9 * * SUN cd /home/kiko/cronjobs/obs_go_scripts/auto_enable_auto_startstop && ./auto_enable_auto_startstop >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_enable_auto_startstop.log 2>&1
#20 9 * * SUN /usr/bin/bash /home/kiko/cronjobs/obs_go_scripts/start_obs.sh
#42 10 * * SUN /usr/bin/bash /home/kiko/cronjobs/obs_go_scripts/stop_obs.sh
#0 11 * * SUN sudo /usr/bin/bash /home/kiko/cronjobs/obs_go_scripts/pi_sleep_wake_till_monday.sh
##wakes up on Monday @ 10:30am
#42 10 * * MON cd /home/kiko/cronjobs/obs_go_scripts/auto_delete_replay/ && ./auto_delete_replay >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_delete_replay.log 2>&1
#42 11 * * MON cd /home/kiko/cronjobs/obs_go_scripts/auto_delete_replay/ && ./auto_delete_replay >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_delete_replay.log 2>&1
#0 12 * * MON sudo /usr/bin/bash /home/kiko/cronjobs/obs_go_scripts/pi_sleep_wake_till_saturday.sh
##wakes up on Saturday @ 7pm
```

Save and exit. To confirm it installed: `crontab -l`.

---

## Configure OBS

### Scenes

Create two scenes in OBS, named **exactly**:

- `LiveScene`
- `PassingSacramentScene`

Capitalisation matters — every piece of code here compares the strings directly.

### obs-websocket

OBS 28+ ships with the WebSocket server built in. In OBS: **Tools → WebSocket Server Settings → Enable WebSocket server**, port `4455`. A password is optional — if you enable one, put it in `.env` as `OBS_WS_PASSWORD`.

Only `scene_switcher` uses this (for the live phone preview). Cron doesn't touch OBS's WebSocket server at all.

### Make OBS react to `scene.txt`

`scene_switcher` writes the current scene name (bare text, no JSON) to `scene_switcher/scene.txt`. OBS needs something on its side that reads the file and switches scenes when it changes. The simplest path is the **Advanced Scene Switcher** plugin:

1. Install it: see https://github.com/WarmUpTill/SceneSwitcher/releases (or `sudo apt install obs-plugins` for an older packaged version).
2. Open **Tools → Advanced Scene Switcher → Macros** and add two macros:
   - **If** "File content of `/home/kiko/cronjobs/obs_go_scripts/scene_switcher/scene.txt` equals `LiveScene`" → **Switch scene to** `LiveScene`.
   - **If** file content equals `PassingSacramentScene` → **Switch scene to** `PassingSacramentScene`.
3. Set the check interval to "For exactly" 1 second.

### Why `start_obs.sh` sets `MESA_GL_VERSION_OVERRIDE=3.3`

Current OBS builds expect OpenGL ≥ 3.3, but the Pi's Mesa driver advertises an older version by default and OBS refuses to start. The env var tells Mesa to report 3.3 and everything works. If you launch OBS manually, use the same prefix:

```bash
MESA_GL_VERSION_OVERRIDE=3.3 obs
```

---

## Scene switcher — the phone control panel

`scene_switcher` is **not** in cron; it runs only while you want the phone UI available. Start it yourself after OBS is up, and just leave it running forever:

```bash
cd ~/cronjobs/obs_go_scripts/scene_switcher
./scene_switcher
```

On startup it prints every URL it's reachable at, including a LAN IP. Open that URL on a phone/tablet on the same Wi-Fi and you'll see a live OBS preview plus two big buttons. Tap one and OBS switches scenes within a second.


---

## Firewall — open port 44330 on the LAN

By default UFW blocks the phone from reaching the control panel on port 44330. Open the port only to your home network (so random people on the internet can't reach it) (make sure to keep ssh port open as well, below, 22 if you're only ssh connecting to the raspberry pi)

```bash
sudo ufw allow 44330
sudo ufw enable
```


If the phone still can't connect after this, double-check it's on the same Wi-Fi network and that `scene_switcher` is actually running (`ps -ef | grep scene_switcher`).

---

## Logs

Every cron entry appends to a file in `logs/`. Quick peek at everything at once:

```bash
tail -f logs/*.log
```

These files are the first place to look when Sunday morning goes sideways.

---

## Troubleshooting

| Symptom                                                                    | Try this first                                                                                                                                     |
| -------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| Any tool prints "401 unauthorized" or "token expired".                     | Re-run `auth_setup`: `cd auth_setup && go run main.go`. That overwrites `../token.json`.                                                           |
| `STREAM_ID ... was not found on your YouTube account`.                     | `auto_schedule_streams` prints the valid stream IDs on that same error — paste the right one into `.env`.                                          |
| Sunday came and no stream went live.                                       | Read `logs/auto_enable_auto_startstop.log`, then confirm OBS actually launched with `ps -ef \| grep obs`. Auto-login to the desktop must be on.    |
| Phone can't open the control panel.                                        | `sudo ufw status` (is the right subnet in the list?), confirm the phone is on the same Wi-Fi, and confirm `scene_switcher` is running.            |
| OBS won't start (OpenGL error).                                            | Make sure the launch command uses `MESA_GL_VERSION_OVERRIDE=3.3`.                                                                                  |
| `scene_switcher` shows "Waiting for OBS stream…" forever.                  | OBS isn't running, or obs-websocket isn't enabled, or your `OBS_WS_PASSWORD` in `.env` doesn't match the one in OBS.                               |
| Cron runs but binaries say "command not found" or similar.                 | Rebuild the binary on the Pi itself (architecture mismatch). Don't copy binaries across machines.                                                  |


# keep laptop running

gnome

`gnome-session-inhibit --inhibit idle:suspend --reason "DND build" sleep infinity`