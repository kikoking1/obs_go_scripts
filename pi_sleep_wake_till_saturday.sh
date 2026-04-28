#!/bin/bash

# ==============================================================================
# Script: shutdown_and_wake.sh
# Purpose: Sets an RTC wake alarm for the next Saturday at 7 PM, then shuts down.
# ==============================================================================

# 1. Calculate the Unix timestamp for the upcoming Saturday at 7:00 PM (21:00)
WAKE_TIME=$(date -d "next Saturday 19:00" +%s)

# 2. Clear any existing alarms first (this is strictly required before setting a new one)
echo 0 | sudo tee /sys/class/rtc/rtc0/wakealarm >/dev/null

# 3. Set the new wake alarm
echo "$WAKE_TIME" | sudo tee /sys/class/rtc/rtc0/wakealarm >/dev/null

# 4. Shut down the Raspberry Pi gracefully
/sbin/shutdown -h now
