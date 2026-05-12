#!/usr/bin/env bash
#
# start_ffmpeg.sh
#
# Capture video from one USB device (by product name, default "UGREEN" —
# the UGREEN HDMI capture card) and audio from another USB device (by
# product name, default "KTB" — the KTB USB Audio interface), then push an
# H.264 + AAC RTMP stream to a YouTube scheduled broadcast. The broadcast
# must already have "Enable Auto Start" + "Enable Auto Stop" turned on, so
# YouTube goes live the moment ffmpeg starts pushing bytes and stops when
# ffmpeg quits.
#
# Configuration read from $REPO_ROOT/.env:
#   STREAM_ID       YouTube stream key (required)
#   VIDEO_USB_NAME  substring of the video device's USB product string
#                   (optional, default "UGREEN")
#   AUDIO_USB_NAME  substring of the audio device's USB product string
#                   (optional, default "KT USB Audio")
#
# Matching is case-insensitive substring on /sys/bus/usb/devices/*/product.
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

VIDEO_USB_NAME="${VIDEO_USB_NAME:-UGREEN}"
AUDIO_USB_NAME="${AUDIO_USB_NAME:-KT USB Audio}"

# --- Find the USB port for each device by product name -------------------
# /sys/bus/usb/devices/<port>/product is the USB product string the device
# advertises (e.g. "UGREEN-25854", "KTB USB Audio"). Matching by name is
# stable across reboots and tells the two devices apart, including when
# one of them (the UGREEN) exposes both video and audio interfaces and we
# only want its video.
list_attached_usb() {
    for dev in /sys/bus/usb/devices/*; do
        [[ -f "$dev/product" ]] || continue
        local port
        port="$(basename "$dev")"
        [[ "$port" =~ ^[0-9]+-[0-9]+(\.[0-9]+)*$ ]] || continue
        echo "  $port: $(cat "$dev/product" 2>/dev/null)"
    done
}

find_usb_port_by_product() {
    local needle lower_needle dev port product lower_product
    needle="$1"
    lower_needle="$(printf '%s' "$needle" | tr '[:upper:]' '[:lower:]')"
    for dev in /sys/bus/usb/devices/*; do
        [[ -f "$dev/product" ]] || continue
        port="$(basename "$dev")"
        # Skip root hubs ("usb1", "usb2", ...) and interface entries ("2-1:1.0").
        [[ "$port" =~ ^[0-9]+-[0-9]+(\.[0-9]+)*$ ]] || continue
        product="$(cat "$dev/product" 2>/dev/null || true)"
        lower_product="$(printf '%s' "$product" | tr '[:upper:]' '[:lower:]')"
        if [[ "$lower_product" == *"$lower_needle"* ]]; then
            echo "$port"
            return 0
        fi
    done
    return 1
}

VIDEO_PORT="$(find_usb_port_by_product "$VIDEO_USB_NAME" || true)"
if [[ -z "$VIDEO_PORT" ]]; then
    echo "ERROR: no USB device whose product name contains '$VIDEO_USB_NAME' is plugged in" >&2
    echo "Currently attached USB devices:" >&2
    list_attached_usb >&2
    exit 1
fi

AUDIO_PORT="$(find_usb_port_by_product "$AUDIO_USB_NAME" || true)"
if [[ -z "$AUDIO_PORT" ]]; then
    echo "ERROR: no USB device whose product name contains '$AUDIO_USB_NAME' is plugged in" >&2
    echo "Currently attached USB devices:" >&2
    list_attached_usb >&2
    exit 1
fi

echo "Video USB port ($VIDEO_USB_NAME): $VIDEO_PORT"
echo "Audio USB port ($AUDIO_USB_NAME): $AUDIO_PORT"

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
    echo "ERROR: no V4L2 video device found on USB port $VIDEO_PORT ($VIDEO_USB_NAME)" >&2
    exit 1
fi

# --- Resolve ALSA hw:CARD on the audio port ------------------------------
# Explicitly use the KTB (or whatever AUDIO_USB_NAME points at) card even
# though the UGREEN also exposes an ALSA card -- we ignore the UGREEN's
# audio entirely.
AUDIO_CARD=""
for s in /sys/class/sound/card*; do
    [[ -e "$s" ]] || continue
    real="$(readlink -f "$s")"
    if [[ "$real" == *"/${AUDIO_PORT}/"* ]]; then
        name="$(basename "$s")"
        AUDIO_CARD="${name#card}"
        break
    fi
done

if [[ -z "$AUDIO_CARD" ]]; then
    echo "ERROR: no ALSA sound card found on USB port $AUDIO_PORT ($AUDIO_USB_NAME)" >&2
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
