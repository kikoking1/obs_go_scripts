# OBS
MESA_GL_VERSION_OVERRIDE=3.3 obs

# Crontabs
```
0 21 * * SAT cd /home/kiko/cronjobs/obs_go_scripts/auto_email_sheets && ./auto_emailsheets >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_emailsheets.log 2>&1
0 21 * * SAT cd /home/kiko/cronjobs/obs_go_scripts/auto_schedule_streams && ./autoschedulestream 1 >> /home/kiko/cronjobs/obs_go_scripts/logs/autoschedulestream.log 2>&1
10 21 * * SAT cd /home/kiko/cronjobs/obs_go_scripts/auto_sync_links_sheets && ./auto_syncsheets >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_syncsheets.log 2>&1
10 9 * * SUN cd /home/kiko/cronjobs/obs_go_scripts/auto_enable_auto_startstop && ./auto_enable_auto_startstop >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_enable_auto_startstop.log 2>&1
20 9 * * SUN /usr/bin/bash /home/kiko/cronjobs/obs_go_scripts/start_obs.sh
42 10 * * SUN /usr/bin/bash /home/kiko/cronjobs/obs_go_scripts/stop_obs.sh
42 10 * * MON cd /home/kiko/cronjobs/obs_go_scripts/auto_delete_replay/ && ./auto_delete_replay >> /home/kiko/cronjobs/obs_go_scripts/logs/auto_delete_replay.log 2>&1
```