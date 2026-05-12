#!/usr/bin/env bash
#
# start_ffmpeg.sh
#
# Capture from the USB 3.0 video device (first USB 3.0 port) and the USB 3.0
# audio device (second USB 3.0 port) and push an H.264 + AAC RTMP stream to
# a YouTube scheduled broadcast. The broadcast must already have
# "Enable Auto Start" + "Enable Auto Stop" turned on, so YouTube goes live
# the moment ffmpeg starts pushing bytes and stops when ffmpeg quits.
#
# The YouTube stream key is read from $REPO_ROOT/.env as STREAM_ID.
#

set -euo pipefail

SCRIPT_DIR="$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )"
LOG_DIR="$SCRIPT_DIR/logs"
LOG_FILE="$LOG_DIR/start_ffmpeg.log"

mkdir -p "$LOG_DIR"
exec > >(tee -a "$LOG_FILE") 2>&1

echo "[$(date -Is)] start_ffmpeg.sh starting"

# --- Load STREAM_ID from .env --------------------------------------------
if [[ ! -f "$SCRIPT_DIR/.env" ]]; then
    echo "ERROR: $SCRIPT_DIR/.env is missing" >&2
    exit 1
fi
set -a
# shellcheck disable=SC1091
source "$SCRIPT_DIR/.env"
set +a

if [[ -z "${STREAM_ID:-}" ]]; then
    echo "ERROR: STREAM_ID is not set in $SCRIPT_DIR/.env" >&2
    exit 1
fi

YOUTUBE_INGEST="rtmp://a.rtmp.youtube.com/live2/${STREAM_ID}"

# --- Find USB 3.0 ports that have a device plugged in --------------------
# /sys/bus/usb/devices/<port>/speed reports the negotiated speed in Mbps.
# USB 3.0 = 5000, USB 3.1 gen2 = 10000, USB 2.0 = 480, USB 1.1 = 12.
# Sorted by port path so the first hit is "the first USB 3.0 port".
mapfile -t USB3_PORTS < <(
    for dev in /sys/bus/usb/devices/*; do
        [[ -f "$dev/speed" ]] || continue
        port="$(basename "$dev")"
        # Skip root hubs ("usb1", "usb2", ...) and interface entries ("2-1:1.0").
        [[ "$port" =~ ^[0-9]+-[0-9]+(\.[0-9]+)*$ ]] || continue
        speed="$(cat "$dev/speed" 2>/dev/null || true)"
        if [[ "$speed" =~ ^[0-9]+$ ]] && (( speed >= 5000 )); then
            echo "$port"
        fi
    done | sort
)

if (( ${#USB3_PORTS[@]} < 2 )); then
    echo "ERROR: expected 2 USB 3.0 devices (video + audio), found ${#USB3_PORTS[@]}: ${USB3_PORTS[*]:-none}" >&2
    exit 1
fi

VIDEO_PORT="${USB3_PORTS[0]}"
AUDIO_PORT="${USB3_PORTS[1]}"

echo "USB 3.0 video port: $VIDEO_PORT"
echo "USB 3.0 audio port: $AUDIO_PORT"

# --- Resolve /dev/videoN on the video port -------------------------------
# A capture card usually registers several /dev/videoN nodes (one per V4L2
# interface). Picking the lowest-numbered one is the convention.
VIDEO_DEV=""
lowest_video=999999
for v in /sys/class/video4linux/video*; do
    [[ -e "$v" ]] || continue
    real="$(readlink -f "$v")"
    if [[ "$real" == *"/${VIDEO_PORT}/"* ]]; then
        name="$(basename "$v")"
        num="${name#video}"
        if [[ "$num" =~ ^[0-9]+$ ]] && (( num < lowest_video )); then
            lowest_video=$num
            VIDEO_DEV="/dev/$name"
        fi
    fi
done

if [[ -z "$VIDEO_DEV" ]]; then
    echo "ERROR: no V4L2 video device found on USB 3.0 port $VIDEO_PORT" >&2
    exit 1
fi

# --- Resolve ALSA hw:CARD on the audio port ------------------------------
AUDIO_CARD=""
for s in /sys/class/sound/card*; do
    [[ -e "$s" ]] || continue
    real="$(readlink -f "$s")"
    if [[ "$real" == *"/${AUDIO_PORT}/"* ]]; then
        AUDIO_CARD="$(basename "$s")"
        AUDIO_CARD="${AUDIO_CARD#card}"
        break
    fi
done

if [[ -z "$AUDIO_CARD" ]]; then
    echo "ERROR: no ALSA sound card found on USB 3.0 port $AUDIO_PORT" >&2
    exit 1
fi

echo "Video device: $VIDEO_DEV"
echo "Audio device: hw:${AUDIO_CARD},0"
echo "Pushing to:   rtmp://a.rtmp.youtube.com/live2/<STREAM_ID>"

# --- Stream --------------------------------------------------------------
# 1080p30, H.264 + AAC, muxed to FLV, sent over RTMP. libx264 with the
# veryfast preset is what fits in a Pi 5's CPU budget at 1080p30 (the Pi 5
# has no hardware H.264 encoder). MJPEG input is what virtually every
# USB 3.0 HDMI capture card actually delivers at 1080p30 -- raw YUYV is
# only available at lower frame rates.
exec ffmpeg -hide_banner -loglevel info \
    -thread_queue_size 1024 \
    -f v4l2 -input_format mjpeg -video_size 1920x1080 -framerate 30 -i "$VIDEO_DEV" \
    -thread_queue_size 1024 \
    -f alsa -ac 2 -ar 48000 -i "hw:${AUDIO_CARD},0" \
    -c:v libx264 -preset veryfast -tune zerolatency -pix_fmt yuv420p \
    -b:v 4500k -maxrate 4500k -bufsize 9000k \
    -g 60 -keyint_min 60 \
    -c:a aac -b:a 128k -ar 48000 \
    -f flv "$YOUTUBE_INGEST"
