#!/bin/bash

# ==============================================================================
# Script: shutdown_and_wake.sh
# Purpose: Sets an RTC wake alarm for the next Saturday at 9 PM, then shuts down.
# ==============================================================================

# 1. Calculate the Unix timestamp for the upcoming Saturday at 9:00 PM (21:00)
WAKE_TIME=$(date -d "next Saturday 19:00" +%s)

# 2. Set the Real-Time Clock (RTC) wake alarm
# -m no : Sets the alarm without changing the system power state immediately
# -t : Specifies the exact target time in seconds since the epoch
/usr/sbin/rtcwake -m no -t "$WAKE_TIME"

# 3. Shut down the Raspberry Pi gracefully
/sbin/shutdown -h now