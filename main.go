package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	_ "time/tzdata" // embed IANA timezone DB so Asia/Tehran works on minimal Linux installs

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

func init() {
	loc, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		loc = time.FixedZone("IRST", 3*3600+30*60)
	}
	time.Local = loc
}

func main() {
	if err := run(); err != nil {
		log.Printf("fatal error: %v", err)
		os.Exit(1)
	}
}

// sessionOpts carries all parameters for a single browser recording attempt.
type sessionOpts struct {
	url             string
	outDir          string
	loadWait        int
	playTimeout     int
	ffmpegPath      string
	displayWidth    int
	displayHeight   int
	debugScreenshot bool
	testMode        bool
	activeDisplay   string
	audioSource     string
	browserPath     string
}

// isRetryableError reports whether err is a transient failure that warrants
// re-launching Chrome and trying the recording again.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, tag := range []string{
		"net::ERR_",
		"ERR_NAME_NOT_RESOLVED",
		"ERR_CONNECTION",
		"ERR_TIMED_OUT",
		"navigation failed",
		"page did not load",
		"start ffmpeg recorder failed",
	} {
		if strings.Contains(s, tag) {
			return true
		}
	}
	return false
}

// recordSession launches Chrome, navigates to the URL, records, and saves the file.
// It is designed to be called inside a retry loop.
func recordSession(ctx context.Context, opts sessionOpts) error {
	l := launcher.New().
		Bin(opts.browserPath).
		Headless(false).
		Leakless(false).
		NoSandbox(true).
		Set("autoplay-policy", "no-user-gesture-required").
		Set("disable-dev-shm-usage").
		Set("no-first-run").
		Set("disable-default-apps").
		Set("disable-extensions")

	if runtime.GOOS == "linux" {
		l = l.
			Set("kiosk").
			Set("disable-gpu").
			Set("window-size", fmt.Sprintf("%d,%d", opts.displayWidth, opts.displayHeight))
	} else {
		l = l.Set("start-maximized")
	}

	u, err := l.Launch()
	if err != nil {
		return fmt.Errorf("launch chrome: %w", err)
	}
	log.Printf("Chrome launched.")

	browser := rod.New().ControlURL(u).Context(ctx).MustConnect()
	defer browser.MustClose()
	log.Printf("Connected to Chrome DevTools.")

	log.Printf("Opening new browser tab...")
	page, pageErr := browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if pageErr != nil {
		return fmt.Errorf("open blank page: %w", pageErr)
	}
	log.Printf("Tab opened. Navigating to URL...")

	// WaitNavigation must be registered BEFORE calling Navigate.
	waitDOMReady := page.WaitNavigation(proto.PageLifecycleEventNameDOMContentLoaded)

	navErr := page.Navigate(opts.url)
	if navErr != nil {
		// Network-level failures are retryable (DNS, connection refused, timeout…).
		// Treat them as hard errors so the caller can retry with a fresh Chrome.
		if isRetryableError(navErr) {
			return fmt.Errorf("navigation failed (retryable): %w", navErr)
		}
		log.Printf("warning: navigate returned non-fatal error (%v), continuing...", navErr)
	}
	log.Printf("Navigate call returned. Waiting for DOMContentLoaded...")

	domDone := make(chan struct{}, 1)
	go func() { waitDOMReady(); domDone <- struct{}{} }()
	select {
	case <-domDone:
		log.Printf("DOMContentLoaded fired.")
	case <-time.After(90 * time.Second):
		log.Printf("warning: DOMContentLoaded not fired within 90s, continuing anyway...")
	}

	// Validate that the page actually loaded (not a Chrome error page).
	if info, infoErr := page.Info(); infoErr == nil {
		pageURL := info.URL
		if !strings.HasPrefix(pageURL, "http://") && !strings.HasPrefix(pageURL, "https://") {
			return fmt.Errorf("page did not load (current URL: %s)", pageURL)
		}
		log.Printf("Page loaded: %s", pageURL)
	}

	if opts.debugScreenshot {
		if img, ssErr := page.Screenshot(false, nil); ssErr != nil {
			log.Printf("debug screenshot failed: %v", ssErr)
		} else {
			ssPath := filepath.Join(".", "debug_screenshot.png")
			if writeErr := os.WriteFile(ssPath, img, 0o644); writeErr != nil {
				log.Printf("debug screenshot write failed: %v", writeErr)
			} else {
				log.Printf("Debug screenshot saved: %s", ssPath)
			}
		}
	}

	wait := time.Duration(opts.loadWait) * time.Second
	log.Printf("Initial wait %s for page stabilization...", wait)
	time.Sleep(wait)

	if err := waitAndClickPlay(page, time.Duration(opts.playTimeout)*time.Second); err != nil {
		log.Printf("warning: play button click skipped/failed: %v", err)
	} else {
		log.Println("Clicked play button when it became visible.")
	}
	if err := ensureMediaPlayback(page); err != nil {
		log.Printf("warning: media playback handshake failed: %v", err)
	}

	if n, err := dismissNotifications(page); err != nil {
		log.Printf("warning: dismiss notifications: %v", err)
	} else if n > 0 {
		log.Printf("Dismissed %d notification(s).", n)
	}

	absOutDir, err := filepath.Abs(opts.outDir)
	if err != nil {
		return fmt.Errorf("cannot resolve output directory: %w", err)
	}
	if err := os.MkdirAll(absOutDir, 0o755); err != nil {
		return fmt.Errorf("cannot create output directory: %w", err)
	}

	var region *captureRegion
	if runtime.GOOS == "linux" {
		time.Sleep(2 * time.Second)
		cr, crErr := getContentBounds(page, opts.displayWidth, opts.displayHeight)
		if crErr != nil {
			log.Printf("warning: content bounds detection failed (%v); capturing full display %dx%d",
				crErr, opts.displayWidth, opts.displayHeight)
		} else {
			region = cr
			log.Printf("Content region detected: x=%d y=%d w=%d h=%d", cr.X, cr.Y, cr.W, cr.H)
		}
	} else {
		region, err = getChromeWindowBounds(browser, page)
		if err != nil {
			log.Printf("warning: could not get Chrome window bounds, recording full desktop: %v", err)
		} else {
			log.Printf("Chrome window region (clamped): x=%d y=%d w=%d h=%d",
				region.X, region.Y, region.W, region.H)
		}
	}

	tempOut := filepath.Join(absOutDir, fmt.Sprintf("_tmp_record_%d.mkv", time.Now().Unix()))
	ffrec, err := startFFmpegDesktopRecorder(opts.ffmpegPath, tempOut, region, opts.activeDisplay, opts.audioSource)
	if err != nil {
		_ = os.Remove(tempOut)
		return fmt.Errorf("start ffmpeg recorder failed: %w", err)
	}
	defer func() { _ = ffrec.Stop(15 * time.Second) }()

	log.Println("Recording started.")
	testSeconds := 0
	if opts.testMode {
		testSeconds = 15
		log.Printf("Test mode: will stop after 15 seconds.")
	}
	if err := waitUntilDone(page, testSeconds); err != nil {
		log.Printf("wait loop warning: %v", err)
	}

	stopErr := ffrec.Stop(3 * time.Minute)
	if stopErr != nil {
		log.Printf("warning: ffmpeg stop returned error: %v", stopErr)
	}
	if !fileExists(tempOut) {
		return fmt.Errorf("ffmpeg did not create output file: %s", tempOut)
	}

	title := urlPathFilename(opts.url)
	if title == "" {
		title = safeFilename(page.MustInfo().Title)
	}
	if title == "" {
		title = "recording"
	}

	outPath := filepath.Join(absOutDir, title+".mp4")
	if remuxErr := remuxToMP4(opts.ffmpegPath, tempOut, outPath); remuxErr != nil {
		log.Printf("warning: remux MKV→MP4 failed (%v); saving as MKV instead", remuxErr)
		mkvOut := filepath.Join(absOutDir, title+".mkv")
		if renErr := os.Rename(tempOut, mkvOut); renErr != nil {
			return fmt.Errorf("could not save recording: remux failed (%v), rename failed (%v)", remuxErr, renErr)
		}
		log.Printf("Saved: %s", mkvOut)
		if stopErr != nil {
			return fmt.Errorf("stop ffmpeg recorder failed: %w", stopErr)
		}
		return nil
	}
	_ = os.Remove(tempOut)
	log.Printf("Saved: %s", outPath)
	if stopErr != nil {
		return fmt.Errorf("stop ffmpeg recorder failed: %w", stopErr)
	}
	return nil
}

func run() error {
	urlFlag := flag.String("url", "", "Adobe Connect playback URL")
	outDirFlag := flag.String("out", "recordings", "Output directory")
	loadWaitFlag := flag.Int("load-wait", 10, "Seconds to wait after page open before clicking play")
	playTimeoutFlag := flag.Int("play-timeout", 120, "Seconds to wait for play button to appear and click")
	logFileFlag := flag.String("log-file", "recorder.log", "Path to log file")
	browserPathFlag := flag.String("browser-path", "", "Path to Chrome/Chromium executable (optional)")
	ffmpegPathFlag := flag.String("ffmpeg-path", "ffmpeg", "Path to ffmpeg executable")
	displayFlag := flag.String("display", "", "X11 display to use on Linux (e.g. :99); leave empty to auto-start Xvfb")
	displayWidthFlag := flag.Int("display-width", 1920, "Xvfb virtual display width (Linux only)")
	displayHeightFlag := flag.Int("display-height", 1080, "Xvfb virtual display height (Linux only)")
	debugScreenshotFlag := flag.Bool("debug-screenshot", false, "Save a screenshot right after page load for debugging")
	testFlag := flag.Bool("test", false, "Test mode: stop recording after 10 seconds")
	flag.Parse()

	if err := setupLogger(*logFileFlag); err != nil {
		return fmt.Errorf("setup logger: %w", err)
	}
	defer log.Println("Program finished.")

	url := strings.TrimSpace(*urlFlag)
	if url == "" {
		return errors.New("usage: .\\adobeconnect-recorder.exe -url \"https://...\"")
	}
	log.Printf("Starting recorder for URL: %s", url)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Xvfb and PulseAudio are set up ONCE outside the retry loop so each retry
	// reuses the same virtual display and audio sink rather than spawning new ones.
	activeDisplay := *displayFlag
	audioSource := ""
	if runtime.GOOS == "linux" {
		disp, dispErr := ensureDisplay(activeDisplay, *displayWidthFlag, *displayHeightFlag)
		if dispErr != nil {
			return fmt.Errorf("ensure X display: %w", dispErr)
		}
		activeDisplay = disp
		log.Printf("Using X display: %s", activeDisplay)
		if setErr := os.Setenv("DISPLAY", activeDisplay); setErr != nil {
			return fmt.Errorf("setenv DISPLAY: %w", setErr)
		}

		mon, audioErr := setupLinuxAudio()
		if audioErr != nil {
			log.Printf("warning: audio setup failed (%v); recording will have no sound", audioErr)
		} else {
			audioSource = mon
			log.Printf("PulseAudio monitor source: %s", audioSource)
			_ = os.Setenv("PULSE_SINK", "chrome_rec")
		}
	}

	browserPath, err := resolveBrowserPath(*browserPathFlag)
	if err != nil {
		return err
	}
	log.Printf("Using browser binary: %s", browserPath)

	opts := sessionOpts{
		url:             url,
		outDir:          *outDirFlag,
		loadWait:        *loadWaitFlag,
		playTimeout:     *playTimeoutFlag,
		ffmpegPath:      *ffmpegPathFlag,
		displayWidth:    *displayWidthFlag,
		displayHeight:   *displayHeightFlag,
		debugScreenshot: *debugScreenshotFlag,
		testMode:        *testFlag,
		activeDisplay:   activeDisplay,
		audioSource:     audioSource,
		browserPath:     browserPath,
	}

	// Retry loop: up to 3 retries (4 total attempts) for transient failures.
	const maxAttempts = 4
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := time.Duration(attempt-1) * 10 * time.Second
			log.Printf("=== Retry %d/%d — waiting %v before next attempt ===",
				attempt-1, maxAttempts-1, delay)
			time.Sleep(delay)
		}
		lastErr = recordSession(ctx, opts)
		if lastErr == nil {
			return nil
		}
		log.Printf("Attempt %d/%d failed: %v", attempt, maxAttempts, lastErr)
		if !isRetryableError(lastErr) {
			log.Printf("Error is not retryable; giving up.")
			return lastErr
		}
	}
	return fmt.Errorf("all %d attempts failed; last error: %w", maxAttempts, lastErr)
}

// remuxToMP4 rewraps src (MKV) into dst (MP4) by copying streams without
// re-encoding. The moov atom is placed at the start for fast web streaming.
func remuxToMP4(ffmpegPath, src, dst string) error {
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	cmd := exec.Command(ffmpegPath, "-y", "-i", src,
		"-c", "copy",
		"-movflags", "+faststart",
		dst,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("exit status: %w\n%s", err, string(out))
	}
	return nil
}

type ffmpegRecorder struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	doneCh    chan error
	outPath   string
	stderrBuf *strings.Builder
}

// setupLinuxAudio starts PulseAudio (if needed), creates a virtual null sink
// so Chrome has somewhere to send audio, and returns the monitor source name
// that ffmpeg should capture from.
// Returns ("", nil) when PulseAudio is unavailable so callers can skip audio.
func setupLinuxAudio() (monitorSource string, err error) {
	if _, lookErr := exec.LookPath("pulseaudio"); lookErr != nil {
		return "", fmt.Errorf("pulseaudio not found in PATH (install with: apt-get install -y pulseaudio)")
	}
	if _, lookErr := exec.LookPath("pactl"); lookErr != nil {
		return "", fmt.Errorf("pactl not found in PATH")
	}

	// Start pulseaudio daemon (idempotent: safe to run when already running).
	startCmd := exec.Command("pulseaudio",
		"--start",
		"--exit-idle-time=-1",
		"--daemon",
		"--log-level=error",
	)
	if out, startErr := startCmd.CombinedOutput(); startErr != nil {
		log.Printf("pulseaudio --start: %v (%s) — may already be running", startErr, strings.TrimSpace(string(out)))
	}
	time.Sleep(800 * time.Millisecond)

	// Create a dedicated null sink for Chrome's audio output.
	// pactl returns the module-id on stdout; ignore "already exists" errors.
	loadOut, loadErr := exec.Command("pactl", "load-module", "module-null-sink",
		"sink_name=chrome_rec",
		"sink_properties=device.description=ChromeRecorder",
	).Output()
	if loadErr != nil {
		log.Printf("pactl load-module: %v — sink may already exist, continuing", loadErr)
	} else {
		log.Printf("PulseAudio null sink loaded (module %s)", strings.TrimSpace(string(loadOut)))
	}

	return "chrome_rec.monitor", nil
}

// ensureDisplay makes sure a valid DISPLAY is available on Linux.
// If displayID is non-empty, it is used as-is.
// Otherwise a new Xvfb instance is started on :99 (or :100, :101 … until one succeeds).
func ensureDisplay(displayID string, width, height int) (string, error) {
	if displayID == "" {
		displayID = os.Getenv("DISPLAY")
	}
	if displayID != "" {
		log.Printf("Re-using existing X display: %s", displayID)
		return displayID, nil
	}

	// Try to start Xvfb on :99, :100, …
	for num := 99; num < 110; num++ {
		disp := fmt.Sprintf(":%d", num)
		cmd := exec.Command("Xvfb", disp,
			"-screen", "0", fmt.Sprintf("%dx%dx24", width, height),
			"-ac",
			"+extension", "GLX",
			"+render",
			"-noreset",
		)
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("cannot start Xvfb (is it installed?): %w", err)
		}
		// Give Xvfb ~1 s to initialize and detect if it exited immediately.
		time.Sleep(1 * time.Second)
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			log.Printf("Xvfb on %s exited immediately, trying next display number", disp)
			continue
		}
		log.Printf("Xvfb started on %s (%dx%d)", disp, width, height)
		// Register cleanup so Xvfb is killed when the recorder exits.
		go func(p *os.Process) {
			// Wait until our process exits, then kill Xvfb.
			// We rely on defer in run() calling cancel(); the goroutine below
			// simply ensures Xvfb does not outlive us.
			_ = p // kept alive by process group; explicit kill via defer in run() not needed here
		}(cmd.Process)
		// Store process for deferred kill (handled via os.Exit path via log.Fatal).
		runtime.SetFinalizer(cmd.Process, func(p *os.Process) { _ = p.Kill() })
		return disp, nil
	}
	return "", errors.New("could not start Xvfb on any display number :99–:109")
}

type captureRegion struct {
	X, Y, W, H int
}

// getContentBounds detects the exact bounding rectangle of the Adobe Connect
// player content by computing the union of all known stable content elements.
// This avoids over-cropping (missing controls) or under-cropping (including whitespace).
// maxW/maxH are the virtual display dimensions used for clamping.
func getContentBounds(page *rod.Page, maxW, maxH int) (*captureRegion, error) {
	res, err := page.Eval(`() => {
		const vpW = window.innerWidth, vpH = window.innerHeight;
		let minX = vpW, minY = vpH, maxX = 0, maxY = 0, found = false;

		const expand = (r, ox, oy) => {
			if (!r || r.width <= 2 || r.height <= 2) return;
			minX = Math.min(minX, r.left + (ox||0));
			minY = Math.min(minY, r.top  + (oy||0));
			maxX = Math.max(maxX, r.right  + (ox||0));
			maxY = Math.max(maxY, r.bottom + (oy||0));
			found = true;
		};

		const scanDoc = (doc, ox, oy) => {
			// Adobe Connect pods have stable IDs: connectPod2, connectPod3, ...
			for (const el of doc.querySelectorAll('[id^="connectPod"]'))
				expand(el.getBoundingClientRect(), ox, oy);

			// Stable recording container IDs.
			for (const id of ['podAreaRecording', 'mainRoomRecording']) {
				const el = doc.getElementById(id);
				if (el) expand(el.getBoundingClientRect(), ox, oy);
			}

			// Play shim button (stable ID) – includes it so controls bar is captured.
			const pb = doc.getElementById('play-recording-shim-button');
			if (pb) expand(pb.getBoundingClientRect(), ox, oy);

			// Time-progress element.
			const te = doc.querySelector('[aria-label^="Time Elapsed"]') ||
			           doc.querySelector('[class*="timeText--"]');
			if (te) expand(te.getBoundingClientRect(), ox, oy);
		};

		scanDoc(document, 0, 0);
		for (const frame of document.querySelectorAll('iframe')) {
			try {
				if (!frame.contentDocument) continue;
				const fr = frame.getBoundingClientRect();
				scanDoc(frame.contentDocument, fr.left, fr.top);
			} catch (_) {}
		}

		if (!found) return null;

		// Clamp to viewport.
		minX = Math.max(0, minX);
		minY = Math.max(0, minY);
		maxX = Math.min(vpW, maxX);
		maxY = Math.min(vpH, maxY);
		const w = maxX - minX, h = maxY - minY;
		if (w < 10 || h < 10) return null;
		return {x: Math.round(minX), y: Math.round(minY),
		        w: Math.round(w),    h: Math.round(h)};
	}`)
	if err != nil {
		return nil, fmt.Errorf("getContentBounds eval: %w", err)
	}
	if res.Value.Nil() || !res.Value.Has("x") {
		return nil, errors.New("content bounds not detected")
	}

	x := int(res.Value.Get("x").Int())
	y := int(res.Value.Get("y").Int())
	w := int(res.Value.Get("w").Int())
	h := int(res.Value.Get("h").Int())
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid content bounds (%dx%d)", w, h)
	}

	// Add extra margin to avoid cutting off right/bottom edges.
	// 1% of display width to the right, 6% of display height to the bottom.
	if maxW > 0 {
		w += maxW * 1 / 100
	}
	if maxH > 0 {
		h += maxH * 6 / 100
	}

	// Clamp to display bounds.
	if x < 0 {
		w += x
		x = 0
	}
	if y < 0 {
		h += y
		y = 0
	}
	if maxW > 0 && x+w > maxW {
		w = maxW - x
	}
	if maxH > 0 && y+h > maxH {
		h = maxH - y
	}
	// Even dimensions required by libx264.
	w = w &^ 1
	h = h &^ 1
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("content region empty after clamping (%dx%d)", w, h)
	}
	return &captureRegion{X: x, Y: y, W: w, H: h}, nil
}

func getChromeWindowBounds(browser *rod.Browser, page *rod.Page) (*captureRegion, error) {
	info, err := page.Info()
	if err != nil {
		return nil, fmt.Errorf("page.Info: %w", err)
	}
	res, err := proto.BrowserGetWindowForTarget{TargetID: info.TargetID}.Call(browser)
	if err != nil {
		return nil, fmt.Errorf("BrowserGetWindowForTarget: %w", err)
	}
	b := res.Bounds
	if b.Width == nil || b.Height == nil {
		return nil, errors.New("window bounds returned nil dimensions")
	}
	x, y := 0, 0
	if b.Left != nil {
		x = *b.Left
	}
	if b.Top != nil {
		y = *b.Top
	}
	w, h := *b.Width, *b.Height
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid window dimensions: %dx%d", w, h)
	}

	// Get physical screen size to clamp the window rect (maximized windows can have
	// negative coordinates on Windows 10/11 due to invisible resize borders).
	screenW, screenH := 0, 0
	if sv, serr := page.Eval(`() => ({w: Math.round(screen.width * devicePixelRatio), h: Math.round(screen.height * devicePixelRatio)})`); serr == nil {
		screenW = int(sv.Value.Get("w").Int())
		screenH = int(sv.Value.Get("h").Int())
	}

	// Clamp to on-screen area.
	if x < 0 {
		w += x // shrink width by the off-screen amount
		x = 0
	}
	if y < 0 {
		h += y
		y = 0
	}
	if screenW > 0 && x+w > screenW {
		w = screenW - x
	}
	if screenH > 0 && y+h > screenH {
		h = screenH - y
	}

	// Ensure even dimensions required by libx264.
	w = w &^ 1
	h = h &^ 1

	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("window region after clamping is empty (%dx%d)", w, h)
	}
	return &captureRegion{X: x, Y: y, W: w, H: h}, nil
}

func startFFmpegDesktopRecorder(ffmpegPath, outPath string, region *captureRegion, display, audioSource string) (*ffmpegRecorder, error) {
	if strings.TrimSpace(ffmpegPath) == "" {
		ffmpegPath = "ffmpeg"
	}
	if _, err := exec.LookPath(ffmpegPath); err != nil {
		// Windows-only fallback path.
		if runtime.GOOS == "windows" {
			fallback := `C:\Users\TA\Documents\ffmpeg\bin\ffmpeg.exe`
			if fileExists(fallback) {
				ffmpegPath = fallback
				log.Printf("ffmpeg not found in PATH, using fallback: %s", ffmpegPath)
			} else {
				return nil, fmt.Errorf("ffmpeg not found (%s) and fallback missing (%s): %w", ffmpegPath, fallback, err)
			}
		} else {
			return nil, fmt.Errorf("ffmpeg not found in PATH (%s): %w", ffmpegPath, err)
		}
	}

	var inputArgs []string
	if runtime.GOOS == "linux" {
		// Linux: use x11grab against the virtual (or real) X display.
		// If a content region was detected, capture only that area.
		disp := display
		if disp == "" {
			disp = os.Getenv("DISPLAY")
		}
		if disp == "" {
			disp = ":0"
		}
		if region != nil {
			// x11grab region syntax: -video_size WxH -i :DISPLAY+X,Y
			dispWithOffset := fmt.Sprintf("%s+%d,%d", disp, region.X, region.Y)
			log.Printf("ffmpeg x11grab region: %s size=%dx%d", dispWithOffset, region.W, region.H)
			inputArgs = []string{
				"-f", "x11grab",
				"-framerate", "30",
				"-draw_mouse", "1",
				"-video_size", fmt.Sprintf("%dx%d", region.W, region.H),
				"-i", dispWithOffset,
			}
		} else {
			log.Printf("ffmpeg x11grab full display %s", disp)
			inputArgs = []string{
				"-f", "x11grab",
				"-framerate", "30",
				"-draw_mouse", "1",
				"-i", disp,
			}
		}
	} else {
		// Windows: gdigrab with optional region offset.
		// gdigrab -i desktop captures the DWM-composited desktop, so GPU-accelerated windows are visible.
		if region != nil {
			log.Printf("ffmpeg region capture: offset=%d,%d size=%dx%d", region.X, region.Y, region.W, region.H)
			inputArgs = []string{
				"-f", "gdigrab",
				"-framerate", "30",
				"-draw_mouse", "1",
				"-offset_x", fmt.Sprintf("%d", region.X),
				"-offset_y", fmt.Sprintf("%d", region.Y),
				"-video_size", fmt.Sprintf("%dx%d", region.W, region.H),
				"-i", "desktop",
			}
		} else {
			log.Printf("No region: recording full desktop.")
			inputArgs = []string{
				"-f", "gdigrab",
				"-framerate", "30",
				"-draw_mouse", "1",
				"-i", "desktop",
			}
		}
	}

	args := []string{"-y"}
	args = append(args, inputArgs...)

	// Audio input: PulseAudio monitor source on Linux (optional).
	hasAudio := audioSource != ""
	if hasAudio {
		log.Printf("ffmpeg audio source: pulse %s", audioSource)
		args = append(args,
			"-f", "pulse",
			"-ac", "2",
			"-i", audioSource,
		)
	}

	// Video codec.
	args = append(args,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-pix_fmt", "yuv420p",
	)
	// Audio codec (only when we have an audio input).
	if hasAudio {
		args = append(args, "-c:a", "aac", "-b:a", "128k")
	}
	// No -movflags here: we record to MKV which writes incrementally and
	// needs no finalization. The moov-to-front step happens in remuxToMP4.
	args = append(args, outPath)

	cmd := exec.Command(ffmpegPath, args...)

	// pipe stdin so we can send "q" to gracefully stop
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdin pipe: %w", err)
	}

	// collect combined stderr+stdout into buffer AND log
	var stderrBuf strings.Builder
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}

	doneCh := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 256*1024), 256*1024)
		// FFmpeg writes status updates with \r (not \n) so each update would
		// otherwise accumulate into one enormous "line" until the next real \n.
		// Treating \r as a line terminator keeps each update as a short line.
		sc.Split(scanCROrLF)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				log.Printf("ffmpeg: %s", line)
				stderrBuf.WriteString(line + "\n")
			}
		}
		doneCh <- cmd.Wait()
	}()

	// Give ffmpeg ~2s to start and fail fast if it can't open the device
	select {
	case err := <-doneCh:
		return nil, fmt.Errorf("ffmpeg exited immediately: %w\nffmpeg output:\n%s", err, stderrBuf.String())
	case <-time.After(2 * time.Second):
	}

	log.Printf("FFmpeg recording started: %s", outPath)
	return &ffmpegRecorder{
		cmd:       cmd,
		stdin:     stdin,
		doneCh:    doneCh,
		outPath:   outPath,
		stderrBuf: &stderrBuf,
	}, nil
}

func (r *ffmpegRecorder) Stop(timeout time.Duration) error {
	if r == nil || r.cmd == nil {
		return nil
	}
	if r.cmd.ProcessState != nil && r.cmd.ProcessState.Exited() {
		select {
		case err := <-r.doneCh:
			if err != nil {
				return fmt.Errorf("ffmpeg exited early: %w", err)
			}
		default:
		}
		if !fileExists(r.outPath) {
			return fmt.Errorf("ffmpeg exited but output file not found: %s", r.outPath)
		}
		return nil
	}

	_, _ = io.WriteString(r.stdin, "q\n")
	_ = r.stdin.Close()

	select {
	case err := <-r.doneCh:
		if err != nil {
			ffLog := ""
			if r.stderrBuf != nil {
				ffLog = "\nffmpeg output:\n" + r.stderrBuf.String()
			}
			return fmt.Errorf("ffmpeg exited with error: %w%s", err, ffLog)
		}
		if !fileExists(r.outPath) {
			return fmt.Errorf("ffmpeg finished without output file: %s", r.outPath)
		}
		return nil
	case <-time.After(timeout):
		_ = r.cmd.Process.Kill()
		return fmt.Errorf("ffmpeg stop timed out after %s", timeout)
	}
}

func resolveBrowserPath(userPath string) (string, error) {
	if p := strings.TrimSpace(userPath); p != "" {
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("browser-path not found: %s", p)
	}

	if runtime.GOOS == "linux" {
		linuxCandidates := []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			"/usr/bin/google-chrome-unstable",
		}
		for _, c := range linuxCandidates {
			if fileExists(c) {
				return c, nil
			}
		}
		// Also try PATH lookup.
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
			if p, err := exec.LookPath(name); err == nil {
				return p, nil
			}
		}
		return "", errors.New("cannot find Chrome/Chromium. Install with: apt-get install -y chromium-browser  OR pass -browser-path")
	}

	localAppData := os.Getenv("LOCALAPPDATA")
	programFiles := os.Getenv("ProgramFiles")
	programFilesX86 := os.Getenv("ProgramFiles(x86)")

	windowsCandidates := []string{
		filepath.Join(programFiles, "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(programFilesX86, "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(localAppData, "Google", "Chrome", "Application", "chrome.exe"),
		filepath.Join(programFiles, "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(programFilesX86, "Microsoft", "Edge", "Application", "msedge.exe"),
		filepath.Join(localAppData, "Microsoft", "Edge", "Application", "msedge.exe"),
	}
	for _, c := range windowsCandidates {
		if fileExists(c) {
			return c, nil
		}
	}
	return "", errors.New("cannot find Chrome/Edge locally. Install Chrome/Edge or pass -browser-path \"C:\\\\path\\\\to\\\\chrome.exe\"")
}

// scanCROrLF is a bufio.SplitFunc that treats \r, \n, and \r\n all as line
// terminators. FFmpeg writes its progress updates using bare \r (overwriting
// the same terminal line), so the default ScanLines (\n only) would accumulate
// thousands of status updates into one huge "line" before the next real \n.
func scanCROrLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\r' || b == '\n' {
			j := i + 1
			// Consume a paired \r\n or \n\r as a single line ending.
			if j < len(data) && (data[j] == '\r' || data[j] == '\n') && data[j] != b {
				j++
			}
			return j, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func fileExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func waitAndClickPlay(page *rod.Page, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		res, err := page.Eval(`() => {
			const isVisible = (el) => {
				if (!el) return false;
				const style = window.getComputedStyle(el);
				const rect = el.getBoundingClientRect();
				return style && style.display !== "none" && style.visibility !== "hidden" &&
					rect.width > 0 && rect.height > 0;
			};

			const clickInDoc = (doc) => {
				if (!doc) return false;
				const btn = doc.getElementById("play-recording-shim-button");
				if (!btn || !isVisible(btn) || btn.disabled) return false;
				btn.click();
				return true;
			};

			if (clickInDoc(document)) return true;

			for (const frame of document.querySelectorAll("iframe")) {
				try {
					if (clickInDoc(frame.contentDocument)) return true;
				} catch (_) {}
			}
			return false;
		}`)
		if err != nil {
			return err
		}
		if res.Value.Bool() {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("play button not found/visible within %s", timeout)
}

func startMediaRecorder(page *rod.Page) error {
	js := `() => new Promise(async (resolve, reject) => {
		try {
			const docs = [document];
			for (const f of document.querySelectorAll("iframe")) {
				try {
					if (f.contentDocument) docs.push(f.contentDocument);
				} catch (_) {}
			}

			let stream = null;
			let source = "";

			for (const d of docs) {
				const v = d.querySelector("video");
				if (v && (v.captureStream || v.mozCaptureStream)) {
					stream = v.captureStream ? v.captureStream() : v.mozCaptureStream?.();
					if (stream) {
						source = "video";
						break;
					}
				}
			}

			if (!stream) {
				for (const d of docs) {
					const c = d.querySelector("canvas");
					if (c && c.captureStream) {
						stream = c.captureStream(30);
						if (stream) {
							source = "canvas";
							break;
						}
					}
				}
			}

			if (!stream) {
				reject("No capturable stream found (video/canvas) in document or iframes.");
				return;
			}

			// If canvas stream has no audio, try to merge audio tracks
			// from any media elements found in same-origin documents.
			let audioTracks = stream.getAudioTracks();
			if (!audioTracks || audioTracks.length === 0) {
				for (const d of docs) {
					const mediaEls = d.querySelectorAll("video, audio");
					for (const m of mediaEls) {
						try {
							const ms = m.captureStream ? m.captureStream() : m.mozCaptureStream?.();
							if (!ms) continue;
							for (const at of ms.getAudioTracks()) {
								try {
									stream.addTrack(at);
								} catch (_) {}
							}
						} catch (_) {}
					}
				}
			}

			const candidates = [
				"video/webm;codecs=vp9,opus",
				"video/webm;codecs=vp8,opus",
				"video/webm"
			];
			let mimeType = "";
			for (const c of candidates) {
				if (MediaRecorder.isTypeSupported(c)) {
					mimeType = c;
					break;
				}
			}

			const chunks = [];
			const recorder = mimeType ? new MediaRecorder(stream, { mimeType }) : new MediaRecorder(stream);
			recorder.ondataavailable = (e) => {
				if (e.data && e.data.size > 0) chunks.push(e.data);
			};
			let recorderError = "";
			recorder.onerror = (e) => {
				recorderError = String((e && e.error && e.error.message) || e?.message || "unknown recorder error");
				console.error("MediaRecorder error", e);
			};

			window.__acr = {
				recorder,
				chunks,
				mimeType: mimeType || "video/webm",
				source,
				videoTrackCount: stream.getVideoTracks().length,
				audioTrackCount: stream.getAudioTracks().length,
				recorderError,
				flushTimer: null,
				startedAt: Date.now()
			};
			recorder.start();
			window.__acr.flushTimer = setInterval(() => {
				try { recorder.requestData(); } catch (_) {}
			}, 2000);
			resolve({
				ok: true,
				source,
				videoTrackCount: stream.getVideoTracks().length,
				audioTrackCount: stream.getAudioTracks().length
			});
		} catch (e) {
			reject(String(e));
		}
	})`

	res, err := page.Eval(js)
	if err == nil {
		log.Printf(
			"Recorder stream source: %s (video tracks=%d, audio tracks=%d)",
			res.Value.Get("source").Str(),
			res.Value.Get("videoTrackCount").Int(),
			res.Value.Get("audioTrackCount").Int(),
		)
		if res.Value.Get("audioTrackCount").Int() == 0 {
			log.Printf("warning: no audio track detected in capturable stream")
		}
	}
	return err
}

func waitUntilDone(page *rod.Page, testSeconds int) error {
	start := time.Now()
	for {
		if testSeconds > 0 && time.Since(start) >= time.Duration(testSeconds)*time.Second {
			log.Printf("Test mode reached %d seconds. Stopping now.", testSeconds)
			return nil
		}

		done, elapsed, total, err := readTimeProgress(page)
		if err == nil {
			log.Printf("Progress: %s / %s", elapsed, total)
			if done {
				log.Println("Playback reached end.")
				return nil
			}
		} else {
			log.Printf("Progress read warning: %v", err)
		}

		time.Sleep(1 * time.Second)
	}
}

func setupLogger(logPath string) error {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	mw := io.MultiWriter(os.Stdout, f)
	log.SetOutput(mw)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("Logging to: %s", logPath)
	return nil
}

func readTimeProgress(page *rod.Page) (done bool, elapsed string, total string, err error) {
	res, evalErr := page.Eval(`() => {
		const fromAria = () => {
			const el = document.querySelector('div[aria-label^="Time Elapsed"]');
			if (!el) return null;
			return {
				text: (el.textContent || "").trim(),
				aria: (el.getAttribute("aria-label") || "").trim()
			};
		};

		const fromClass = () => {
			const el = document.querySelector('div[class*="timeText--"]');
			if (!el) return null;
			return {
				text: (el.textContent || "").trim(),
				aria: (el.getAttribute("aria-label") || "").trim()
			};
		};

		const fromDoc = (doc) => {
			if (!doc) return null;
			const e1 = doc.querySelector('div[aria-label^="Time Elapsed"]');
			if (e1) {
				return {
					text: (e1.textContent || "").trim(),
					aria: (e1.getAttribute("aria-label") || "").trim()
				};
			}
			const e2 = doc.querySelector('div[class*="timeText--"]');
			if (e2) {
				return {
					text: (e2.textContent || "").trim(),
					aria: (e2.getAttribute("aria-label") || "").trim()
				};
			}
			return null;
		};

		let out = fromAria() || fromClass();
		if (out) return out;

		for (const frame of document.querySelectorAll("iframe")) {
			try {
				out = fromDoc(frame.contentDocument);
				if (out) return out;
			} catch (_) {}
		}

		return { text: "", aria: "" };
	}`)
	if evalErr != nil {
		err = evalErr
		return
	}

	text := strings.TrimSpace(res.Value.Get("text").Str())
	aria := strings.TrimSpace(res.Value.Get("aria").Str())

	e1, t1 := parseElapsedTotalFromText(text)
	if e1 == "" || t1 == "" {
		e1, t1 = parseElapsedTotalFromAria(aria)
	}
	if e1 == "" || t1 == "" {
		err = errors.New("could not parse elapsed/total time yet")
		return
	}

	elapsed = e1
	total = t1
	done = elapsed == total
	return
}

func stopRecorder(page *rod.Page) ([]byte, string, error) {
	js := `() => new Promise(async (resolve, reject) => {
		try {
			if (!window.__acr || !window.__acr.recorder) {
				reject("Recorder state not found.");
				return;
			}

			const recorder = window.__acr.recorder;
			const chunks = window.__acr.chunks || [];
			if (window.__acr.flushTimer) {
				try { clearInterval(window.__acr.flushTimer); } catch (_) {}
			}

			recorder.ondataavailable = (e) => {
				if (e.data && e.data.size > 0) chunks.push(e.data);
			};

			recorder.onstop = async () => {
				try {
					if (!chunks.length) {
						reject("recorder stopped but no chunks captured; recorderError=" + (window.__acr.recorderError || ""));
						return;
					}
					const blob = new Blob(chunks, { type: window.__acr.mimeType || "video/webm" });
					const ab = await blob.arrayBuffer();
					const bytes = new Uint8Array(ab);
					if (!bytes.length) {
						reject("recorder produced empty blob");
						return;
					}
					let bin = "";
					const step = 0x8000;
					for (let i = 0; i < bytes.length; i += step) {
						bin += String.fromCharCode(...bytes.subarray(i, i + step));
					}
					const b64 = btoa(bin);
					resolve({ ok: true, base64: b64, mimeType: window.__acr.mimeType || "video/webm" });
				} catch (e) {
					reject(String(e));
				}
			};

			if (recorder.state === "inactive") {
				recorder.onstop();
				return;
			}

			try { recorder.requestData(); } catch (_) {}
			setTimeout(() => {
				try { recorder.stop(); } catch (e) { reject(String(e)); }
			}, 1200);
		} catch (e) {
			reject(String(e));
		}
	})`

	res, err := page.Eval(js)
	if err != nil {
		return nil, "", err
	}

	b64 := res.Value.Get("base64").Str()
	mime := res.Value.Get("mimeType").Str()
	if b64 == "" {
		return nil, "", errors.New("empty base64 output from recorder")
	}

	data, decodeErr := base64.StdEncoding.DecodeString(b64)
	if decodeErr != nil {
		return nil, "", decodeErr
	}
	return data, mime, nil
}

// dismissNotifications closes any visible dismissible alert/notification banners
// (e.g. the "Switch to Classic View" banner in Adobe Connect).
// Returns the number of Close buttons clicked.
func dismissNotifications(page *rod.Page) (int, error) {
	res, err := page.Eval(`() => {
		let count = 0;
		const docs = [document];
		for (const f of document.querySelectorAll("iframe")) {
			try { if (f.contentDocument) docs.push(f.contentDocument); } catch (_) {}
		}
		for (const doc of docs) {
			// Click every "Close" button inside a dismissible notifier.
			for (const notifier of doc.querySelectorAll('[data-is-dismissible="true"]')) {
				for (const btn of notifier.querySelectorAll("button")) {
					if (btn.textContent.trim() === "Close") {
						try { btn.click(); count++; } catch (_) {}
					}
				}
			}
			// Also try the specific Adobe Connect close button by ID pattern.
			const closeBtn = doc.querySelector('[id$="_0"].spectrum-Button');
			if (closeBtn && closeBtn.textContent.trim() === "Close") {
				try { closeBtn.click(); count++; } catch (_) {}
			}
		}
		return count;
	}`)
	if err != nil {
		return 0, err
	}
	return int(res.Value.Int()), nil
}

func ensureMediaPlayback(page *rod.Page) error {
	_, err := page.Eval(`() => {
		const docs = [document];
		for (const f of document.querySelectorAll("iframe")) {
			try {
				if (f.contentDocument) docs.push(f.contentDocument);
			} catch (_) {}
		}

		for (const d of docs) {
			for (const m of d.querySelectorAll("video, audio")) {
				try {
					m.muted = false;
					if (typeof m.volume === "number") m.volume = 1;
					const p = m.play?.();
					if (p && typeof p.catch === "function") p.catch(() => {});
				} catch (_) {}
			}
		}
		return true;
	}`)
	return err
}

func parseElapsedTotalFromText(v string) (string, string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", ""
	}
	parts := strings.Split(v, "/")
	if len(parts) != 2 {
		return "", ""
	}
	return normalizeClock(parts[0]), normalizeClock(parts[1])
}

func parseElapsedTotalFromAria(v string) (string, string) {
	// Example:
	// "Time Elapsed, 00 hours, 00 minutes, 00 seconds of 01 hours, 19 minutes, 04 seconds"
	re := regexp.MustCompile(`(?i)(\d{1,2})\s*hours?,\s*(\d{1,2})\s*minutes?,\s*(\d{1,2})\s*seconds?\s*of\s*(\d{1,2})\s*hours?,\s*(\d{1,2})\s*minutes?,\s*(\d{1,2})\s*seconds?`)
	m := re.FindStringSubmatch(v)
	if len(m) != 7 {
		return "", ""
	}
	elapsed := fmt.Sprintf("%02s:%02s:%02s", m[1], m[2], m[3])
	total := fmt.Sprintf("%02s:%02s:%02s", m[4], m[5], m[6])
	return normalizeClock(elapsed), normalizeClock(total)
}

func normalizeClock(v string) string {
	v = strings.TrimSpace(v)
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return v
	}
	return fmt.Sprintf("%02s:%02s:%02s", strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]))
}

// urlPathFilename extracts the last non-empty path segment from a URL and
// sanitises it for use as a filename.
// e.g. "https://vc11.sbu.ac.ir/pv0k1icomhs3/?session=..." → "pv0k1icomhs3"
func urlPathFilename(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return ""
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(segments[i]); s != "" {
			return safeFilename(s)
		}
	}
	return ""
}

func safeFilename(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	illegal := regexp.MustCompile(`[<>:"/\\|?*\x00-\x1F]`)
	v = illegal.ReplaceAllString(v, "_")
	nonASCII := regexp.MustCompile(`[^\x20-\x7E]`)
	v = nonASCII.ReplaceAllString(v, "_")
	multiUnderscore := regexp.MustCompile(`_+`)
	v = multiUnderscore.ReplaceAllString(v, "_")
	v = strings.Trim(v, ". ")
	if v == "" {
		return "recording"
	}
	return v
}

func extensionFromMime(mime string) string {
	switch {
	case strings.Contains(mime, "webm"):
		return ".webm"
	case strings.Contains(mime, "mp4"):
		return ".mp4"
	default:
		return ".webm"
	}
}
