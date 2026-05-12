#!/usr/bin/env bash
#
# stop_ffmpeg.sh
#
# End the Sunday service stream. SIGTERM first so ffmpeg writes the FLV
# trailer and YouTube sees a clean end-of-stream (auto-stop then closes
# the broadcast). SIGKILL after 10 s as a safety net.
#
# Targets only ffmpeg processes pushing to YouTube's RTMP ingest, so it
# won't trample any unrelated ffmpeg jobs that might be running.
#

PATTERN='ffmpeg.*rtmp://a\.rtmp\.youtube\.com/live2/'

pkill -TERM -f "$PATTERN"

sleep 10
pkill -KILL -f "$PATTERN"
