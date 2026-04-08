# Adobe Connect Recorder

Records Adobe Connect class sessions on a headless Linux server.

## Dependencies

```bash
sudo apt install -y xvfb ffmpeg pulseaudio pulseaudio-utils
```

## Install Google Chrome

```bash
wget https://downloader.s3.ir-thr-at1.arvanstorage.ir/google-chrome-stable_current_amd64.deb
sudo apt install -y ./google-chrome-stable_current_amd64.deb
```

## Usage

**Test (stops after 15 seconds):**
```bash
./adobeconnectrecorder_linux \
  -url "https://vc11.sbu.ac.ir/pv0k1icomhs3/?session=7u9nkfrx2cpkhoe" \
  -out recordings \
  -log-file recorder.log \
  -test
```

**Full recording:**
```bash
./adobeconnectrecorder_linux \
  -url "https://vc11.sbu.ac.ir/pv0k1icomhs3/?session=7u9nkfrx2cpkhoe" \
  -out recordings \
  -log-file recorder.log
```

Output file is saved as `recordings/<session-id>.mp4`.

## Notes

- The recorder automatically starts Xvfb and PulseAudio if they are not running.
- Recording stops automatically when the session reaches its end.
- Check `recorder.log` for progress and any errors.
