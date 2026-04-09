# Adobe Connect Recorder

Headless recorder for Adobe Connect sessions, with a web-based job manager.

---

## Dependencies

```bash
sudo apt install -y xvfb ffmpeg pulseaudio pulseaudio-utils
```

**Google Chrome:**
```bash
wget https://downloader.s3.ir-thr-at1.arvanstorage.ir/google-chrome-stable_current_amd64.deb
sudo apt install -y ./google-chrome-stable_current_amd64.deb
```

---

## Web Server (recommended)

Manages up to N concurrent recorder instances with a Persian RTL web UI on port 7040.

**Run:**
```bash
./webserver_linux \
  -recorder ./adobeconnectrecorder_linux \
  -out      ./recordings \
  -slots    4 \
  -font-dir /var/www/botstudio.ir/fonts \
  -port     7040
```

**All flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-recorder` | `./adobeconnectrecorder_linux` | Path to recorder binary |
| `-out` | `./recordings` | Output directory |
| `-slots` | `4` | Max concurrent recordings (1–16) |
| `-port` | `7040` | HTTP listen port |
| `-log-dir` | `./job_logs` | Per-job log directory |
| `-font-dir` | `/var/www/botstudio.ir/fonts` | Vazirmatn font directory |
| `-state` | `./webserver_state.json` | State persistence file |

**Features:**
- Submit a URL, course name, and session — jobs queue automatically
- Live log streaming and progress bar per job
- Duplicate URL detection: re-submitting a recorded URL shows the download link
- Failed jobs can be re-submitted; successfully recorded URLs are blocked from re-recording
- Stall detection: kills a job automatically if no progress for 90 seconds
- Orphan Chrome/FFmpeg cleanup when no jobs are active
- State persists across server restarts (`webserver_state.json`)
- All timestamps in Tehran time (Asia/Tehran)

---

## Recorder (standalone)

**Test — stops after 15 seconds:**
```bash
./adobeconnectrecorder_linux \
  -url      "https://vc11.sbu.ac.ir/pv0k1icomhs3/?session=7u9nkfrx2cpkhoe" \
  -out      recordings \
  -log-file recorder.log \
  -test
```

**Full recording:**
```bash
./adobeconnectrecorder_linux \
  -url      "https://vc11.sbu.ac.ir/pv0k1icomhs3/?session=7u9nkfrx2cpkhoe" \
  -out      recordings \
  -log-file recorder.log
```

**All flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | *(required)* | Adobe Connect playback URL |
| `-out` | `recordings` | Output directory |
| `-log-file` | `recorder.log` | Log file path |
| `-test` | false | Stop after 15 seconds |
| `-ffmpeg-path` | `ffmpeg` | Path to ffmpeg binary |
| `-browser-path` | *(auto)* | Path to Chrome/Chromium |
| `-display` | *(auto)* | X11 display (e.g. `:99`); auto-starts Xvfb if empty |
| `-display-width` | `1920` | Xvfb width |
| `-display-height` | `1080` | Xvfb height |
| `-load-wait` | `10` | Seconds to wait after page load before clicking play |
| `-play-timeout` | `120` | Seconds to wait for play button |
| `-debug-screenshot` | false | Save `debug_screenshot.png` after page load |

---

## Notes

- Recording format: **MKV during capture → remuxed to MP4** after completion. If FFmpeg is killed mid-recording the MKV is still playable (just truncated).
- On network errors (DNS, connection refused) the recorder retries automatically up to **3 times** with increasing delays.
- Output filename is derived from the URL path segment (e.g. `pv0k1icomhs3.mp4`).
- Check `recorder.log` or the web UI for live progress.
