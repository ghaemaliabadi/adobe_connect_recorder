package main

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // embed IANA timezone DB so Asia/Tehran works on minimal Linux installs
)

func init() {
	loc, err := time.LoadLocation("Asia/Tehran")
	if err != nil {
		loc = time.FixedZone("IRST", 3*3600+30*60)
	}
	time.Local = loc
}

// ──────────────────────────────────────────────────────────────────────────────
// Config vars (set by flags)
// ──────────────────────────────────────────────────────────────────────────────

var (
	port        string
	recorderBin string
	outDir      string
	stateFile   string
	logDir      string
	fontDir     string
	maxSlots    int
)

const maxLogLines = 200

// ──────────────────────────────────────────────────────────────────────────────
// Data types
// ──────────────────────────────────────────────────────────────────────────────

type JobStatus string

const (
	StatusQueued  JobStatus = "queued"
	StatusRunning JobStatus = "running"
	StatusDone    JobStatus = "done"
	StatusFailed  JobStatus = "failed"
)

// stallThreshold is how long elapsed must be unchanged before a job is
// considered stalled (no progress). Intentionally generous.
const stallThreshold = 90 * time.Second

type Job struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	URLHash     string    `json:"url_hash"`
	BaseURLHash string    `json:"base_url_hash"` // hashURL(url) — same for test & non-test jobs
	CourseName  string    `json:"course_name"`
	Session     string    `json:"session"`
	TestMode    bool      `json:"test_mode"`
	Status      JobStatus `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	FinishedAt  time.Time `json:"finished_at,omitempty"`
	OutputFile  string    `json:"output_file,omitempty"`
	Elapsed     string    `json:"elapsed"`
	Total       string    `json:"total"`
	ErrorMsg    string    `json:"error,omitempty"`

	// Runtime-only (not persisted in state.json — stored in per-job log files)
	logMu         sync.Mutex
	logLines      []string
	lastElapsedAt time.Time // when Elapsed last changed
	stalled       bool      // true when no progress for stallThreshold
	killIssued    bool      // ensures we only send the kill signal once
	killCause     string    // human-readable reason for an intentional kill
	cmdMu         sync.Mutex
	cmd           *exec.Cmd // the live recorder subprocess (nil when not running)
}

// JobView is the serialisable snapshot sent over SSE / REST.
type JobView struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	URLHash    string    `json:"url_hash"`
	CourseName string    `json:"course_name"`
	Session    string    `json:"session"`
	TestMode   bool      `json:"test_mode"`
	Status     JobStatus `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	OutputFile string    `json:"output_file,omitempty"`
	Elapsed    string    `json:"elapsed"`
	Total      string    `json:"total"`
	ErrorMsg   string    `json:"error,omitempty"`
	Stalled    bool      `json:"stalled"`
	LogLines   []string  `json:"log_lines"`
}

func (j *Job) view() JobView {
	j.logMu.Lock()
	defer j.logMu.Unlock()
	lines := make([]string, len(j.logLines))
	copy(lines, j.logLines)
	return JobView{
		ID:         j.ID,
		URL:        j.URL,
		URLHash:    j.URLHash,
		CourseName: j.CourseName,
		Session:    j.Session,
		TestMode:   j.TestMode,
		Status:     j.Status,
		CreatedAt:  j.CreatedAt,
		StartedAt:  j.StartedAt,
		FinishedAt: j.FinishedAt,
		OutputFile: j.OutputFile,
		Elapsed:    j.Elapsed,
		Total:      j.Total,
		ErrorMsg:   j.ErrorMsg,
		Stalled:    j.stalled,
		LogLines:   lines,
	}
}

func (j *Job) appendLog(line string) {
	j.logMu.Lock()
	defer j.logMu.Unlock()
	j.logLines = append(j.logLines, line)
	if len(j.logLines) > maxLogLines {
		j.logLines = j.logLines[len(j.logLines)-maxLogLines:]
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Log file helpers (persist per-job logs across restarts)
// ──────────────────────────────────────────────────────────────────────────────

func jobLogPath(id string) string {
	return filepath.Join(logDir, "job_"+id+".log")
}

const (
	maxLogLineBytes = 1024          // truncate individual lines longer than this
	maxLogFileBytes = 2 * 1024 * 1024 // rotate log file when it exceeds 2 MB
	keepLogLines    = 500           // lines to keep after rotation
)

// safeLogLine ensures a single log line never exceeds maxLogLineBytes.
func safeLogLine(line string) string {
	if len(line) > maxLogLineBytes {
		return line[:maxLogLineBytes] + " …[truncated]"
	}
	return line
}

func writeLogLine(id, line string) {
	line = safeLogLine(line)
	path := jobLogPath(id)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	fmt.Fprintln(f, line)
	f.Close()

	// Rotate if the file has grown too large.
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogFileBytes {
		rotateLogFile(path)
	}
}

// rotateLogFile keeps only the last keepLogLines lines to cap disk usage.
func rotateLogFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > keepLogLines {
		lines = lines[len(lines)-keepLogLines:]
	}
	_ = os.WriteFile(path, bytes.Join(lines, []byte("\n")), 0644)
}

func loadLogFile(job *Job) {
	data, err := os.ReadFile(jobLogPath(job.ID))
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	job.logMu.Lock()
	defer job.logMu.Unlock()
	if len(lines) > maxLogLines {
		lines = lines[len(lines)-maxLogLines:]
	}
	job.logLines = lines
}

// ──────────────────────────────────────────────────────────────────────────────
// SSE hub
// ──────────────────────────────────────────────────────────────────────────────

type sseHub struct {
	mu      sync.RWMutex
	clients map[chan []byte]struct{}
}

func newSSEHub() *sseHub {
	return &sseHub{clients: make(map[chan []byte]struct{})}
}

func (h *sseHub) subscribe() chan []byte {
	ch := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *sseHub) broadcast(event, data string) {
	msg := []byte("event: " + event + "\ndata: " + data + "\n\n")
	h.mu.RLock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default:
		}
	}
	h.mu.RUnlock()
}

// ──────────────────────────────────────────────────────────────────────────────
// Manager
// ──────────────────────────────────────────────────────────────────────────────

type Manager struct {
	mu      sync.Mutex
	jobs    []*Job
	byID    map[string]*Job
	byHash  map[string]*Job
	running int

	hub       *sseHub
	saveTimer *time.Timer
}

var mgr *Manager

func newManager() *Manager {
	return &Manager{
		byID:   make(map[string]*Job),
		byHash: make(map[string]*Job),
		hub:    newSSEHub(),
	}
}

func generateID() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func hashURL(u string) string {
	u = strings.TrimSpace(u)
	h := md5.Sum([]byte(u))
	return fmt.Sprintf("%x", h)
}

// hashURLTest gives each test run its own unique hash so test jobs never
// collide with real jobs or with each other.
func hashURLTest(u string) string {
	key := strings.TrimSpace(u) + fmt.Sprintf("~test~%d", time.Now().UnixNano())
	h := md5.Sum([]byte(key))
	return fmt.Sprintf("%x~t", h)
}

// Add enqueues a new job or returns an existing one for the same URL.
// For non-test jobs the full duplicate check (including done/failed) is applied.
// For test-mode jobs, only concurrent (running/queued) jobs for the same URL
// are blocked — re-testing after completion is always allowed.
func (m *Manager) Add(url, courseName, session string, testMode bool) (*Job, bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	baseHash := hashURL(url)

	// Regardless of test mode, never allow two concurrent jobs for the same URL.
	// We check BaseURLHash (the URL's canonical hash, independent of test mode)
	// to block a new job when the same URL is already running or queued.
	for _, j := range m.jobs {
		if j.BaseURLHash == baseHash {
			if j.Status == StatusRunning || j.Status == StatusQueued {
				return j, true, true
			}
		}
	}

	var hash string
	if testMode {
		hash = hashURLTest(url)
	} else {
		hash = baseHash
		if prev, ok := m.byHash[hash]; ok {
			switch prev.Status {
			case StatusRunning, StatusQueued:
				// Already handled by the concurrent-job loop above; guard here too.
				return prev, true, true
			case StatusDone:
				// Successfully recorded — tell the user and offer the download link.
				return prev, true, false
			case StatusFailed:
				// Previous attempt failed (no usable file). Allow a fresh recording.
				// Fall through to create a new job; update byHash to the new entry.
			}
		}
	}

	job := &Job{
		ID:          generateID(),
		URL:         url,
		URLHash:     hash,
		BaseURLHash: baseHash,
		CourseName:  courseName,
		Session:     session,
		TestMode:    testMode,
		Status:      StatusQueued,
		CreatedAt:   time.Now(),
	}
	m.jobs = append([]*Job{job}, m.jobs...)
	m.byID[job.ID] = job
	m.byHash[hash] = job

	m.scheduleSave()
	go m.tryStart()
	return job, false, false
}

func (m *Manager) tryStart() {
	m.mu.Lock()
	if m.running >= maxSlots {
		m.mu.Unlock()
		return
	}
	var next *Job
	for i := len(m.jobs) - 1; i >= 0; i-- {
		if m.jobs[i].Status == StatusQueued {
			next = m.jobs[i]
			break
		}
	}
	if next == nil {
		m.mu.Unlock()
		return
	}
	next.Status = StatusRunning
	next.StartedAt = time.Now()
	m.running++
	m.mu.Unlock()

	m.broadcastState()
	go m.runJob(next)
}

var progressRe = regexp.MustCompile(`Progress:\s*(\d{2}:\d{2}:\d{2})\s*/\s*(\d{2}:\d{2}:\d{2})`)

func (m *Manager) runJob(job *Job) {
	defer func() {
		m.mu.Lock()
		m.running--
		m.scheduleSave()
		m.mu.Unlock()
		m.broadcastState()
		go m.tryStart()
	}()

	absOut, err := filepath.Abs(outDir)
	if err != nil {
		m.finishJob(job, "", fmt.Sprintf("out dir error: %v", err))
		return
	}

	args := []string{"-url", job.URL, "-out", absOut}
	if job.TestMode {
		args = append(args, "-test")
	}
	cmd := exec.Command(recorderBin, args...)
	cmd.Dir = filepath.Dir(recorderBin)
	// Put the recorder in its own process group so we can kill the whole tree
	// (Chrome + FFmpeg + Xvfb) with a single signal on Linux.
	setSysProcAttr(cmd)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		m.finishJob(job, "", fmt.Sprintf("stdout pipe: %v", err))
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		m.finishJob(job, "", fmt.Sprintf("start recorder: %v", err))
		return
	}

	// Store the running cmd so the stall detector can kill it.
	job.cmdMu.Lock()
	job.cmd = cmd
	job.cmdMu.Unlock()
	defer func() {
		job.cmdMu.Lock()
		job.cmd = nil
		job.cmdMu.Unlock()
	}()

	// Initialise stall tracker: start counting from job start.
	mgr.mu.Lock()
	job.lastElapsedAt = time.Now()
	mgr.mu.Unlock()

	var lastOutputFile string
	reader := bufio.NewReaderSize(stdoutPipe, 4*1024*1024)
	for {
		raw, err := reader.ReadString('\n')
		// Process whatever was read before the error/EOF.
		if len(raw) > 0 {
			line := strings.TrimRight(raw, "\r\n")
			line = safeLogLine(line)

			job.appendLog(line)
			writeLogLine(job.ID, line)

			if m2 := progressRe.FindStringSubmatch(line); len(m2) == 3 {
				mgr.mu.Lock()
				if job.Elapsed != m2[1] {
					// Elapsed advanced — reset stall clock.
					job.Elapsed = m2[1]
					job.lastElapsedAt = time.Now()
					if job.stalled {
						job.stalled = false
					}
				}
				job.Total = m2[2]
				mgr.mu.Unlock()
			}

			// Detect the final output path from the recorder's "Saved: /path/file.ext" log line.
			if strings.Contains(line, "Saved:") {
				for _, p := range strings.Fields(line) {
					if strings.HasSuffix(p, ".mp4") || strings.HasSuffix(p, ".mkv") {
						lastOutputFile = filepath.Base(p)
					}
				}
			}

			// Detect expired session marker emitted by the recorder.
			if strings.Contains(line, "[SESSION_EXPIRED]") {
				mgr.mu.Lock()
				job.ErrorMsg = "سشن منقضی شده — لینک جدیدی از Adobe Connect دریافت کنید"
				mgr.mu.Unlock()
				// Broadcast a dedicated alert so the frontend can show a prominent banner.
				if alertEvt, err := json.Marshal(map[string]string{
					"id":      job.ID,
					"type":    "session_expired",
					"course":  job.CourseName,
					"session": job.Session,
				}); err == nil {
					mgr.hub.broadcast("alert", string(alertEvt))
				}
			}

			logEvent, _ := json.Marshal(map[string]string{"id": job.ID, "line": line})
			mgr.hub.broadcast("log", string(logEvent))
		}
		if err != nil {
			break // EOF or pipe closed
		}
	}

	if err := cmd.Wait(); err != nil {
		m.finishJob(job, lastOutputFile, fmt.Sprintf("recorder exited with error: %v", err))
		return
	}

	if lastOutputFile == "" {
		lastOutputFile = guessOutputFile(job.URL, absOut)
	}
	m.finishJob(job, lastOutputFile, "")
}

func (m *Manager) finishJob(job *Job, outputFile, errMsg string) {
	m.mu.Lock()
	job.FinishedAt = time.Now()
	job.OutputFile = outputFile
	job.stalled = false
	cause := job.killCause
	if cause != "" {
		job.Status = StatusFailed
		job.ErrorMsg = cause
	} else if errMsg != "" {
		job.Status = StatusFailed
		job.ErrorMsg = errMsg
	} else {
		job.Status = StatusDone
	}
	m.mu.Unlock()
}

// killJob terminates the recorder subprocess and its entire process group
// (Chrome, FFmpeg, Xvfb). Safe to call from any goroutine.
func (m *Manager) killJob(job *Job) {
	job.cmdMu.Lock()
	c := job.cmd
	job.cmdMu.Unlock()
	if c == nil {
		return
	}
	killProcessGroup(c)
}

// checkStalls marks running jobs as stalled when their elapsed time hasn't
// changed for stallThreshold. Each job tracks its own clock independently,
// so concurrent jobs are evaluated separately.
// When a job is first detected as stalled, it is killed automatically.
func (m *Manager) checkStalls() {
	m.mu.Lock()
	var toKill []*Job
	changed := false
	for _, j := range m.jobs {
		if j.Status != StatusRunning || j.lastElapsedAt.IsZero() {
			continue
		}
		was := j.stalled
		j.stalled = time.Since(j.lastElapsedAt) > stallThreshold
		if j.stalled && !j.killIssued {
			j.killIssued = true
			j.killCause = "متوقف شد: ۹۰ ثانیه بدون پیشرفت در ویدیو"
			toKill = append(toKill, j)
		}
		if j.stalled != was {
			changed = true
		}
	}
	m.mu.Unlock()

	for _, j := range toKill {
		msg := "⏹ جاب به‌دلیل عدم پیشرفت به‌مدت ۹۰ ثانیه به‌طور خودکار متوقف و کشته شد"
		j.appendLog(msg)
		writeLogLine(j.ID, msg)
		if ev, err := json.Marshal(map[string]string{"id": j.ID, "line": msg}); err == nil {
			m.hub.broadcast("log", string(ev))
		}
		go m.killJob(j)
	}

	if changed || len(toKill) > 0 {
		m.broadcastState()
	}
}

func guessOutputFile(rawURL, absOut string) string {
	seg := urlPathSegment(rawURL)
	if seg != "" {
		for _, ext := range []string{".mp4", ".mkv"} {
			candidate := filepath.Join(absOut, seg+ext)
			if _, err := os.Stat(candidate); err == nil {
				return seg + ext
			}
		}
	}
	return ""
}

func urlPathSegment(rawURL string) string {
	if idx := strings.Index(rawURL, "?"); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	rawURL = strings.TrimRight(rawURL, "/")
	if idx := strings.LastIndex(rawURL, "/"); idx >= 0 {
		return rawURL[idx+1:]
	}
	return ""
}

func (m *Manager) broadcastState() {
	m.mu.Lock()
	views := make([]JobView, 0, len(m.jobs))
	for _, j := range m.jobs {
		views = append(views, j.view())
	}
	running := m.running
	slots := maxSlots
	m.mu.Unlock()

	type statePayload struct {
		Jobs    []JobView `json:"jobs"`
		Running int       `json:"running"`
		MaxCon  int       `json:"max_concurrent"`
	}
	data, _ := json.Marshal(statePayload{Jobs: views, Running: running, MaxCon: slots})
	m.hub.broadcast("state", string(data))
}

// ──────────────────────────────────────────────────────────────────────────────
// State persistence
// ──────────────────────────────────────────────────────────────────────────────

type persistedState struct {
	Jobs []*Job `json:"jobs"`
}

func (m *Manager) scheduleSave() {
	if m.saveTimer != nil {
		m.saveTimer.Stop()
	}
	m.saveTimer = time.AfterFunc(500*time.Millisecond, func() {
		if err := m.save(); err != nil {
			log.Printf("save state: %v", err)
		}
	})
}

func (m *Manager) save() error {
	m.mu.Lock()
	state := persistedState{Jobs: m.jobs}
	m.mu.Unlock()
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

func (m *Manager) load() error {
	data, err := os.ReadFile(stateFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	absOut, _ := filepath.Abs(outDir)

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range state.Jobs {
		if j.Status == StatusRunning || j.Status == StatusQueued {
			// Check whether the recording file was saved before the crash.
			found := false
			if j.OutputFile != "" {
				if _, err := os.Stat(filepath.Join(absOut, j.OutputFile)); err == nil {
					found = true
				}
			}
			if !found {
				// Try to guess from URL path segment (check mp4 and mkv).
				seg := urlPathSegment(j.URL)
				if seg != "" {
					for _, ext := range []string{".mp4", ".mkv"} {
						candidate := filepath.Join(absOut, seg+ext)
						if _, err := os.Stat(candidate); err == nil {
							j.OutputFile = seg + ext
							found = true
							break
						}
					}
				}
			}
			if found {
				j.Status = StatusDone
				if j.FinishedAt.IsZero() {
					j.FinishedAt = time.Now()
				}
			} else {
				j.Status = StatusFailed
				j.ErrorMsg = "سرور ری‌استارت شد"
				j.FinishedAt = time.Now()
			}
		}

		// Back-fill BaseURLHash for jobs saved before this field was added.
		if j.BaseURLHash == "" {
			j.BaseURLHash = hashURL(j.URL)
		}

		// Reload persisted log lines from log file.
		loadLogFile(j)

		m.jobs = append(m.jobs, j)
		m.byID[j.ID] = j
		m.byHash[j.URLHash] = j
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP handlers
// ──────────────────────────────────────────────────────────────────────────────

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, indexHTML)
}

type recordRequest struct {
	URL        string `json:"url"`
	CourseName string `json:"course_name"`
	Session    string `json:"session"`
	TestMode   bool   `json:"test_mode"`
}

type recordResponse struct {
	OK          bool     `json:"ok"`
	Message     string   `json:"message"`
	Job         *JobView `json:"job,omitempty"`
	Duplicate   bool     `json:"duplicate"`
	InProgress  bool     `json:"in_progress"`
	DownloadURL string   `json:"download_url,omitempty"`
}

func handleRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req recordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResp(w, 400, recordResponse{OK: false, Message: "JSON نامعتبر"})
		return
	}
	req.URL = strings.TrimSpace(req.URL)
	if req.URL == "" {
		jsonResp(w, 400, recordResponse{OK: false, Message: "آدرس جلسه الزامی است"})
		return
	}
	if req.CourseName == "" {
		jsonResp(w, 400, recordResponse{OK: false, Message: "نام درس الزامی است"})
		return
	}

	job, isDup, inProgress := mgr.Add(req.URL, req.CourseName, req.Session, req.TestMode)
	v := job.view()

	if isDup {
		resp := recordResponse{OK: true, Duplicate: true, InProgress: inProgress, Job: &v}
		switch {
		case inProgress:
			resp.Message = "این آدرس در حال حاضر در صف ضبط است."
		case job.Status == StatusDone && job.OutputFile != "":
			ts := time.Now().Unix()
			resp.DownloadURL = fmt.Sprintf("/download/%s?t=%d", job.OutputFile, ts)
			resp.Message = "این جلسه قبلاً ضبط شده. لینک دانلود آماده است."
		case job.Status == StatusDone && job.OutputFile == "":
			// Recorded successfully but file is missing — treat as re-recordable
			// (shouldn't normally happen; Add() would have let this through).
			resp.Message = "ضبط قبلی فایل خروجی ندارد. لطفاً دوباره ثبت کنید."
		default:
			resp.Message = "یک تلاش قبلی برای این آدرس وجود دارد."
		}
		jsonResp(w, 200, resp)
		return
	}

	mgr.broadcastState()
	jsonResp(w, 200, recordResponse{OK: true, Message: "ضبط در صف قرار گرفت.", Job: &v})
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	mgr.mu.Lock()
	views := make([]JobView, 0, len(mgr.jobs))
	for _, j := range mgr.jobs {
		views = append(views, j.view())
	}
	mgr.mu.Unlock()
	jsonResp(w, 200, map[string]interface{}{"jobs": views, "max_concurrent": maxSlots})
}

// FileEntry is the JSON shape returned by /api/files.
type FileEntry struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	CourseName string    `json:"course_name,omitempty"`
	Session    string    `json:"session,omitempty"`
	JobID      string    `json:"job_id,omitempty"`
}

func handleFiles(w http.ResponseWriter, r *http.Request) {
	absOut, _ := filepath.Abs(outDir)

	// Build a filename → job lookup once, while holding the lock briefly.
	mgr.mu.Lock()
	jobByFile := make(map[string]*Job, len(mgr.jobs))
	for _, j := range mgr.jobs {
		if j.OutputFile != "" {
			jobByFile[j.OutputFile] = j
		}
	}
	mgr.mu.Unlock()

	entries, err := os.ReadDir(absOut)
	if err != nil {
		jsonResp(w, 500, map[string]string{"error": "cannot read recordings directory"})
		return
	}

	var files []FileEntry
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		name := de.Name()
		if !strings.HasSuffix(name, ".mp4") && !strings.HasSuffix(name, ".mkv") {
			continue
		}
		// Skip temp files that are still being written.
		if strings.HasPrefix(name, "_tmp_record_") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		fe := FileEntry{
			Name:       name,
			Size:       info.Size(),
			ModifiedAt: info.ModTime(),
		}
		if j, ok := jobByFile[name]; ok {
			fe.CourseName = j.CourseName
			fe.Session = j.Session
			fe.JobID = j.ID
		}
		files = append(files, fe)
	}

	// Sort newest first.
	for i, j := 0, len(files)-1; i < j; i, j = i+1, j-1 {
		files[i], files[j] = files[j], files[i]
	}
	if files == nil {
		files = []FileEntry{}
	}

	w.Header().Set("Cache-Control", "no-store")
	jsonResp(w, 200, map[string]interface{}{"files": files})
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := mgr.hub.subscribe()
	defer mgr.hub.unsubscribe(ch)

	mgr.broadcastState()

	for {
		select {
		case msg, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "%s", msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	filename := strings.TrimPrefix(r.URL.Path, "/download/")
	if filename == "" || strings.Contains(filename, "..") || strings.Contains(filename, "/") {
		http.Error(w, "invalid filename", 400)
		return
	}
	// Accept only known video extensions.
	if !strings.HasSuffix(filename, ".mp4") && !strings.HasSuffix(filename, ".mkv") {
		http.Error(w, "unsupported file type", 400)
		return
	}
	absOut, err := filepath.Abs(outDir)
	if err != nil {
		http.Error(w, "server error", 500)
		return
	}
	fullPath := filepath.Join(absOut, filename)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		http.Error(w, "file not found", 404)
		return
	}
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	http.ServeFile(w, r, fullPath)
}

func jsonResp(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ──────────────────────────────────────────────────────────────────────────────
// Entry point
// ──────────────────────────────────────────────────────────────────────────────

func main() {
	flag.StringVar(&port, "port", "7040", "HTTP listen port")
	flag.StringVar(&recorderBin, "recorder", "./adobeconnectrecorder_linux", "Path to recorder binary")
	flag.StringVar(&outDir, "out", "./recordings", "Output directory for recordings")
	flag.StringVar(&stateFile, "state", "./webserver_state.json", "State persistence file")
	flag.StringVar(&logDir, "log-dir", "./job_logs", "Directory for per-job log files")
	flag.StringVar(&fontDir, "font-dir", "/var/www/botstudio.ir/fonts", "Directory to serve fonts from (/fonts/)")
	flag.IntVar(&maxSlots, "slots", 4, "Max concurrent recordings (1-16)")
	flag.Parse()

	if maxSlots < 1 {
		maxSlots = 1
	}
	if maxSlots > 16 {
		maxSlots = 16
	}

	for _, d := range []string{outDir, logDir} {
		abs, err := filepath.Abs(d)
		if err != nil {
			log.Fatalf("resolve dir %s: %v", d, err)
		}
		if err := os.MkdirAll(abs, 0755); err != nil {
			log.Fatalf("create dir %s: %v", abs, err)
		}
		switch d {
		case outDir:
			outDir = abs
		case logDir:
			logDir = abs
		}
	}
	if abs, err := filepath.Abs(recorderBin); err == nil {
		recorderBin = abs
	}
	if abs, err := filepath.Abs(stateFile); err == nil {
		stateFile = abs
	}

	mgr = newManager()
	if err := mgr.load(); err != nil {
		log.Printf("warn: load state: %v", err)
	}

	// Periodic state broadcast keeps SSE alive.
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for range tick.C {
			mgr.broadcastState()
		}
	}()

	// Stall detector: checks every 20 s whether any running job has stopped advancing.
	go func() {
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for range tick.C {
			mgr.checkStalls()
		}
	}()

	// Orphan Chrome cleanup: when no jobs are active, kill any leftover Chrome
	// processes that may have survived due to bugs or ungraceful shutdowns.
	go func() {
		tick := time.NewTicker(60 * time.Second)
		defer tick.Stop()
		for range tick.C {
			mgr.mu.Lock()
			hasActive := false
			for _, j := range mgr.jobs {
				if j.Status == StatusRunning || j.Status == StatusQueued {
					hasActive = true
					break
				}
			}
			mgr.mu.Unlock()
			if !hasActive {
				killOrphanChromes()
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/api/record", handleRecord)
	mux.HandleFunc("/api/jobs", handleJobs)
	mux.HandleFunc("/api/files", handleFiles)
	mux.HandleFunc("/api/stream", handleStream)
	mux.HandleFunc("/download/", handleDownload)

	// Serve font files (Vazirmatn)
	if fontDir != "" {
		if _, err := os.Stat(fontDir); err == nil {
			fs := http.FileServer(http.Dir(fontDir))
			mux.Handle("/fonts/", http.StripPrefix("/fonts/", fs))
			log.Printf("Serving fonts from: %s", fontDir)
		} else {
			log.Printf("warn: font-dir not found: %s (fonts will not be served)", fontDir)
		}
	}

	addr := ":" + port
	log.Printf("Web server listening on http://localhost%s", addr)
	log.Printf("Recorder binary : %s", recorderBin)
	log.Printf("Output dir      : %s", outDir)
	log.Printf("Log dir         : %s", logDir)
	log.Printf("Max slots       : %d", maxSlots)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Embedded HTML (Persian, RTL, Vazirmatn)
// ──────────────────────────────────────────────────────────────────────────────

const indexHTML = `<!DOCTYPE html>
<html lang="fa" dir="rtl">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ضبط‌کننده Adobe Connect</title>
<style>
@font-face{font-family:'Vazirmatn';font-style:normal;font-weight:400;src:url('/fonts/Vazir_p30download.com.eot');src:url('/fonts/Vazir_p30download.com.eot?#iefix') format('embedded-opentype'),url('/fonts/Vazir_p30download.com.woff2') format('woff2'),url('/fonts/Vazir_p30download.com.woff') format('woff'),url('/fonts/Vazir_p30download.com.ttf') format('truetype');font-display:swap}
@font-face{font-family:'Vazirmatn';font-style:normal;font-weight:700;src:url('/fonts/Vazir-Bold_p30download.com.eot');src:url('/fonts/Vazir-Bold_p30download.com.eot?#iefix') format('embedded-opentype'),url('/fonts/Vazir-Bold_p30download.com.woff2') format('woff2'),url('/fonts/Vazir-Bold_p30download.com.woff') format('woff'),url('/fonts/Vazir-Bold_p30download.com.ttf') format('truetype');font-display:swap}

*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
:root{
  --bg:#0d1117;
  --surface:#161b22;
  --surface2:#21262d;
  --surface3:#2d333b;
  --border:#30363d;
  --text:#e6edf3;
  --text2:#8b949e;
  --text3:#6e7681;
  --accent:#58a6ff;
  --accent2:#1f6feb;
  --green:#3fb950;
  --red:#f85149;
  --yellow:#e3b341;
  --purple:#bc8cff;
  --orange:#ffa657;
  --radius:10px;
  --shadow:0 4px 24px rgba(0,0,0,.5);
}
html{font-size:15px}
body{background:var(--bg);color:var(--text);font-family:'Vazirmatn',-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;min-height:100vh;padding:24px;direction:rtl;text-align:right}
h1,h2,h3{font-weight:700}
a{color:var(--accent);text-decoration:none}
a:hover{text-decoration:underline}

.container{max-width:1300px;margin:0 auto}
.header{display:flex;align-items:center;gap:12px;padding-bottom:20px;border-bottom:1px solid var(--border);margin-bottom:28px}
.header h1{font-size:1.5rem;background:linear-gradient(135deg,#58a6ff,#bc8cff);-webkit-background-clip:text;-webkit-text-fill-color:transparent;background-clip:text}
.logo{width:36px;height:36px;background:linear-gradient(135deg,#58a6ff,#bc8cff);border-radius:8px;display:flex;align-items:center;justify-content:center;font-size:18px;flex-shrink:0}
.subtitle{color:var(--text2);font-size:.85rem;margin-top:2px}

.grid-main{display:grid;grid-template-columns:340px 1fr;gap:24px;align-items:start}
@media(max-width:900px){.grid-main{grid-template-columns:1fr}}

.card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);padding:20px;box-shadow:var(--shadow)}
.card-title{font-size:.8rem;font-weight:700;letter-spacing:.04em;color:var(--text2);margin-bottom:16px}

.form-group{margin-bottom:14px}
label{display:block;font-size:.85rem;color:var(--text2);margin-bottom:5px}
input[type=text],input[type=url]{width:100%;padding:9px 12px;background:var(--surface2);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:.9rem;outline:none;transition:border-color .2s;font-family:inherit;direction:ltr;text-align:left}
input:focus{border-color:var(--accent)}
input::placeholder{color:var(--text3);direction:rtl;text-align:right}
.btn{display:inline-flex;align-items:center;justify-content:center;gap:7px;padding:9px 18px;border:none;border-radius:6px;font-size:.9rem;font-weight:600;cursor:pointer;transition:all .15s;font-family:inherit}
.btn-primary{background:linear-gradient(135deg,#1f6feb,#388bfd);color:#fff}
.btn-primary:hover{filter:brightness(1.15)}
.btn-primary:disabled{opacity:.5;cursor:not-allowed}
.btn-full{width:100%}
.spinner{width:14px;height:14px;border:2px solid rgba(255,255,255,.3);border-top-color:#fff;border-radius:50%;animation:spin .7s linear infinite}
@keyframes spin{to{transform:rotate(360deg)}}

.slots-bar{display:flex;gap:6px;margin-bottom:18px;direction:ltr}
.slot{flex:1;height:6px;border-radius:3px;background:var(--surface3);transition:background .4s}
.slot.active{background:linear-gradient(90deg,#58a6ff,#bc8cff)}
.slots-label{font-size:.78rem;color:var(--text2);margin-bottom:6px}

.jobs-area{display:flex;flex-direction:column;gap:14px}
.no-jobs{text-align:center;color:var(--text3);padding:40px 0;font-size:.9rem}

.job-card{background:var(--surface);border:1px solid var(--border);border-radius:var(--radius);overflow:hidden;transition:border-color .3s}
.job-card.running{border-color:rgba(88,166,255,.35)}
.job-card.done{border-color:rgba(63,185,80,.25)}
.job-card.failed{border-color:rgba(248,81,73,.25)}

.job-header{display:flex;align-items:center;gap:10px;padding:13px 16px;border-bottom:1px solid var(--border)}
.job-badge{font-size:.7rem;font-weight:700;padding:3px 8px;border-radius:20px;white-space:nowrap}
.badge-queued{background:rgba(139,148,158,.15);color:var(--text2)}
.badge-running{background:rgba(88,166,255,.15);color:var(--accent)}
.badge-done{background:rgba(63,185,80,.15);color:var(--green)}
.badge-failed{background:rgba(248,81,73,.15);color:var(--red)}
.badge-test{background:rgba(255,166,87,.15);color:var(--orange);font-size:.65rem;padding:2px 6px;border-radius:10px;font-weight:700;vertical-align:middle;margin-right:4px;border:1px solid rgba(255,166,87,.3)}

.job-name{font-weight:600;font-size:.9rem;flex:1;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.job-session{font-size:.78rem;color:var(--text2);white-space:nowrap;direction:rtl}
.job-meta{padding:10px 16px;display:flex;align-items:center;gap:10px;flex-wrap:wrap}
.job-url{font-size:.73rem;color:var(--text3);word-break:break-all;flex:1;direction:ltr;text-align:left}

.progress-wrap{padding:0 16px 10px;direction:ltr}
.progress-times{display:flex;justify-content:space-between;font-size:.75rem;color:var(--text2);margin-bottom:4px;font-family:monospace;direction:ltr}
.progress-bar{height:4px;background:var(--surface3);border-radius:2px;overflow:hidden;direction:ltr}
.progress-fill{height:100%;background:linear-gradient(90deg,#58a6ff,#bc8cff);border-radius:2px;transition:width .8s ease;min-width:0;float:left}
.running .progress-fill{animation:pulse-bar 2s ease-in-out infinite alternate}
@keyframes pulse-bar{from{filter:brightness(1)}to{filter:brightness(1.4)}}
.stall-warn{font-size:.72rem;color:var(--yellow);padding:4px 16px 6px;display:flex;align-items:center;gap:5px;direction:rtl}
.stall-warn::before{content:'⚠';font-size:.85rem}

.dot{width:8px;height:8px;border-radius:50%;flex-shrink:0}
.dot-queued{background:var(--text3)}
.dot-running{background:var(--accent);animation:pulse-dot 1.4s ease-in-out infinite}
.dot-stalled{background:var(--yellow);animation:pulse-dot 1s ease-in-out infinite}
.dot-done{background:var(--green)}
.dot-failed{background:var(--red)}
@keyframes pulse-dot{0%,100%{opacity:1;transform:scale(1)}50%{opacity:.5;transform:scale(.7)}}
.job-card.stalled{border-color:rgba(227,179,65,.4)!important}

.log-area{background:#0d1117;border-top:1px solid var(--border);padding:10px 14px;font-family:'Courier New',Courier,monospace;font-size:.72rem;color:#4ec9b0;height:130px;overflow-y:auto;line-height:1.55;direction:ltr;text-align:left}
.log-area .log-line{word-break:break-all}
.log-area .log-err{color:var(--red)}
.log-area .log-prog{color:var(--yellow)}
.log-empty{color:var(--text3);font-style:italic;direction:rtl;text-align:right}

.dl-wrap{padding:10px 16px}
.btn-dl{background:rgba(63,185,80,.15);color:var(--green);border:1px solid rgba(63,185,80,.3);padding:6px 14px;border-radius:6px;font-size:.82rem;font-weight:600;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center;gap:6px;transition:background .2s;font-family:inherit}
.btn-dl:hover{background:rgba(63,185,80,.25);text-decoration:none}
.job-error{padding:8px 16px;font-size:.78rem;color:var(--red);border-top:1px solid var(--border)}

/* Toggle switch */
.toggle-wrap{display:flex;align-items:center;gap:10px;cursor:pointer;user-select:none}
.toggle-wrap input[type=checkbox]{position:absolute;opacity:0;width:0;height:0}
.toggle-slider{position:relative;display:inline-block;width:38px;height:20px;background:var(--surface3);border-radius:10px;transition:background .2s;flex-shrink:0;border:1px solid var(--border)}
.toggle-slider::after{content:'';position:absolute;top:2px;right:2px;width:14px;height:14px;background:#fff;border-radius:50%;transition:transform .2s}
.toggle-wrap input:checked+.toggle-slider{background:var(--orange);border-color:var(--orange)}
.toggle-wrap input:checked+.toggle-slider::after{transform:translateX(-18px)}
.toggle-label{font-size:.85rem;color:var(--text2)}
.test-chip{background:rgba(255,166,87,.15);color:var(--orange);border:1px solid rgba(255,166,87,.3);font-size:.68rem;padding:1px 6px;border-radius:10px;font-weight:700;vertical-align:middle;margin-right:3px}

.history-section{margin-top:32px}
.history-section h2{font-size:.95rem;color:var(--text2);margin-bottom:14px;display:flex;align-items:center;gap:8px}
.history-section h2::before{content:'';flex:1;height:1px;background:var(--border)}
.table-wrap{overflow-x:auto}
table{width:100%;border-collapse:collapse;font-size:.82rem}
th{color:var(--text2);font-weight:600;text-align:right;padding:8px 12px;border-bottom:1px solid var(--border);white-space:nowrap}
td{padding:9px 12px;border-bottom:1px solid rgba(48,54,61,.5);color:var(--text);text-align:right}
tr:last-child td{border-bottom:none}
tr:hover td{background:rgba(255,255,255,.02)}
.td-url{max-width:220px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--text3);direction:ltr;text-align:left}
.td-date{white-space:nowrap;color:var(--text2)}
.no-history{text-align:center;color:var(--text3);padding:24px;font-size:.85rem}

/* ── Global search ─────────────────────────────────────────── */
.global-search-wrap{display:flex;align-items:center;gap:10px;padding:12px 16px;border-bottom:1px solid var(--border);background:var(--surface2);border-radius:var(--radius) var(--radius) 0 0}
.global-search-icon{color:var(--text3);font-size:1rem;flex-shrink:0}
.global-search-wrap input{flex:1;background:transparent;border:none;outline:none;font-size:.92rem;color:var(--text);font-family:inherit;direction:rtl}
.global-search-wrap input::placeholder{color:var(--text3)}
.global-search-badge{font-size:.72rem;background:var(--surface3);color:var(--text2);border-radius:20px;padding:2px 9px;white-space:nowrap}
.global-search-clear{background:none;border:none;color:var(--text3);cursor:pointer;font-size:.95rem;padding:0 2px;line-height:1;display:none}
.global-search-clear.visible{display:block}

.toast-area{position:fixed;top:20px;left:20px;z-index:9999;display:flex;flex-direction:column;gap:10px;max-width:360px}
.toast{padding:12px 16px;border-radius:8px;font-size:.85rem;box-shadow:var(--shadow);display:flex;align-items:flex-start;gap:10px;animation:slide-in .2s ease}
@keyframes slide-in{from{opacity:0;transform:translateX(-20px)}to{opacity:1;transform:translateX(0)}}
.toast-info{background:#1c2432;border:1px solid var(--accent2);color:var(--text)}
.toast-success{background:#14261e;border:1px solid rgba(63,185,80,.4);color:var(--text)}
.toast-warn{background:#261e14;border:1px solid rgba(227,179,65,.4);color:var(--text)}
.toast-error{background:#261414;border:1px solid rgba(248,81,73,.4);color:var(--text)}
.toast-icon{font-size:1rem;line-height:1}
.toast-close{margin-right:auto;cursor:pointer;color:var(--text3);background:none;border:none;font-size:1rem;line-height:1;padding:0}

.conn-dot{width:8px;height:8px;border-radius:50%;background:var(--red);margin-right:auto;flex-shrink:0;transition:background .3s}
.conn-dot.connected{background:var(--green);animation:pulse-dot 2s ease-in-out infinite}
.conn-label{font-size:.72rem;color:var(--text3)}

/* ── File browser ──────────────────────────────────────────── */
.files-section{margin-top:32px}
.files-section h2{font-size:.95rem;color:var(--text2);margin-bottom:14px;display:flex;align-items:center;gap:8px}
.files-section h2::before{content:'';flex:1;height:1px;background:var(--border)}
.search-bar{display:flex;gap:10px;align-items:center;padding:14px 16px;border-bottom:1px solid var(--border)}
.search-bar input{flex:1;padding:8px 12px;background:var(--surface2);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:.85rem;font-family:inherit;outline:none;direction:rtl}
.search-bar input:focus{border-color:var(--accent)}
.search-bar .file-count{font-size:.78rem;color:var(--text3);white-space:nowrap}
.files-table td.td-name{font-family:monospace;font-size:.8rem;text-align:right;color:var(--accent)}
.files-table td.td-size{white-space:nowrap;color:var(--text2);font-size:.78rem;text-align:right}
.files-table th{text-align:right}
.files-table .td-course{max-width:200px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.files-table .td-session{color:var(--text2);font-size:.78rem;white-space:nowrap}
.no-files{text-align:center;color:var(--text3);padding:24px;font-size:.85rem}
</style>
</head>
<body>
<div class="container">

  <div class="header">
    <div class="logo">&#127909;</div>
    <div>
      <h1>ضبط‌کننده Adobe Connect</h1>
      <div class="subtitle">مدیریت خودکار ضبط جلسات آنلاین</div>
    </div>
    <div style="margin-right:auto;display:flex;align-items:center;gap:6px">
      <div class="conn-label" id="connLabel">قطع</div>
      <div class="conn-dot" id="connDot"></div>
    </div>
  </div>

  <div class="grid-main">

    <!-- Sidebar -->
    <div>
      <div class="card">
        <div class="card-title">&#43; ضبط جدید</div>

        <div class="slots-label" id="slotsLabel">در حال بارگذاری&#8230;</div>
        <div class="slots-bar" id="slotsBar">
          <div class="slot" id="s0"></div>
          <div class="slot" id="s1"></div>
          <div class="slot" id="s2"></div>
          <div class="slot" id="s3"></div>
        </div>

        <form id="recForm" novalidate>
          <div class="form-group">
            <label for="fUrl">آدرس جلسه *</label>
            <input type="url" id="fUrl" placeholder="https://vc11.sbu.ac.ir/pv0k1icomhs3/?session=..." autocomplete="off">
          </div>
          <div class="form-group">
            <label for="fCourse">نام درس *</label>
            <input type="text" id="fCourse" placeholder="مثلاً: طراحی سیستم‌های دیجیتال" autocomplete="off" style="direction:rtl;text-align:right">
          </div>
          <div class="form-group">
            <label for="fSession">شماره جلسه</label>
            <input type="text" id="fSession" placeholder="مثلاً: جلسه ۳" autocomplete="off" style="direction:rtl;text-align:right">
          </div>
          <div class="form-group">
            <label class="toggle-wrap" for="fTest">
              <input type="checkbox" id="fTest">
              <span class="toggle-slider"></span>
              <span class="toggle-label">حالت تست <span class="test-chip">۱۰ ثانیه</span></span>
            </label>
          </div>
          <button type="submit" class="btn btn-primary btn-full" id="submitBtn">
            <span id="submitIcon">&#9654;</span>
            <span id="submitLabel">شروع ضبط</span>
          </button>
        </form>
      </div>

      <div class="card" style="margin-top:16px">
        <div class="card-title">آمار</div>
        <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px">
          <div style="text-align:center">
            <div style="font-size:1.8rem;font-weight:700;color:var(--accent)" id="statRunning">0</div>
            <div style="font-size:.75rem;color:var(--text2)">در حال ضبط</div>
          </div>
          <div style="text-align:center">
            <div style="font-size:1.8rem;font-weight:700;color:var(--yellow)" id="statQueued">0</div>
            <div style="font-size:.75rem;color:var(--text2)">در صف</div>
          </div>
          <div style="text-align:center">
            <div style="font-size:1.8rem;font-weight:700;color:var(--green)" id="statDone">0</div>
            <div style="font-size:.75rem;color:var(--text2)">انجام شد</div>
          </div>
          <div style="text-align:center">
            <div style="font-size:1.8rem;font-weight:700;color:var(--red)" id="statFailed">0</div>
            <div style="font-size:.75rem;color:var(--text2)">خطا</div>
          </div>
        </div>
      </div>
    </div>

    <!-- Main area -->
    <div>
      <div class="card-title" style="margin-bottom:12px">جاب‌های فعال</div>
      <div class="jobs-area" id="jobsArea">
        <div class="no-jobs" id="noJobs">جاب فعالی وجود ندارد. برای شروع ضبط یک آدرس وارد کنید.</div>
      </div>
    </div>

  </div>

  <div class="history-section">
    <h2>ضبط‌های اخیر</h2>
    <div class="card" style="padding:0">
      <div class="table-wrap">
        <table id="histTable">
          <thead>
            <tr>
              <th>نام درس</th>
              <th>جلسه</th>
              <th>وضعیت</th>
              <th>پایان</th>
              <th>مدت</th>
              <th>دانلود</th>
            </tr>
          </thead>
          <tbody id="histBody">
            <tr><td colspan="6" class="no-history">در حال بارگذاری&#8230;</td></tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>

  <div class="files-section">
    <h2>همه فایل‌های ضبط شده روی دیسک</h2>
    <div class="card" style="padding:0">
      <div class="search-bar">
        <span style="color:var(--text3);font-size:.9rem;flex-shrink:0">📁</span>
        <input type="text" id="fileSearch" placeholder="فیلتر بر اساس نام فایل، درس یا جلسه…" oninput="filterFiles()" style="direction:rtl">
        <span class="file-count" id="fileCount">در حال بارگذاری…</span>
        <button class="btn" style="padding:6px 11px;background:var(--surface2);border:1px solid var(--border);color:var(--text2);font-size:.82rem;flex-shrink:0" onclick="loadFiles()" title="بروزرسانی لیست فایل‌ها">&#8635;</button>
      </div>
      <div class="table-wrap">
        <table class="files-table" id="filesTable">
          <thead>
            <tr>
              <th>نام درس</th>
              <th>جلسه</th>
              <th>نام فایل</th>
              <th>حجم</th>
              <th>تاریخ</th>
              <th>دانلود</th>
            </tr>
          </thead>
          <tbody id="filesBody">
            <tr><td colspan="6" class="no-files">در حال بارگذاری&#8230;</td></tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>

</div>

<div class="toast-area" id="toastArea"></div>

<script>
'use strict';

let jobs = [];
let running = 0;
let maxConcurrent = 4;

// ─── SSE ─────────────────────────────────────────────────────────────────────
let evtSource = null;
let reconnectTimer = null;

function connectSSE() {
  if (evtSource) { evtSource.close(); evtSource = null; }
  evtSource = new EventSource('/api/stream');

  evtSource.addEventListener('state', e => {
    const data = JSON.parse(e.data);
    jobs = data.jobs || [];
    running = data.running || 0;
    maxConcurrent = data.max_concurrent || 4;
    render();
    setConnected(true);
  });

  evtSource.addEventListener('log', e => {
    const { id, line } = JSON.parse(e.data);
    const job = jobs.find(j => j.id === id);
    if (job) {
      if (!job.log_lines) job.log_lines = [];
      job.log_lines.push(line);
      if (job.log_lines.length > 200) job.log_lines.splice(0, job.log_lines.length - 200);
      const m = line.match(/Progress:\s*(\d{2}:\d{2}:\d{2})\s*\/\s*(\d{2}:\d{2}:\d{2})/);
      if (m) { job.elapsed = m[1]; job.total = m[2]; }
      appendLogLine(id, line);
      updateProgress(job);
    }
  });

  evtSource.addEventListener('alert', e => {
    const data = JSON.parse(e.data);
    if (data.type === 'session_expired') {
      const label = [data.course, data.session].filter(Boolean).join(' — ') || data.id;
      toast(
        '⚠️ <strong>سشن منقضی شده</strong><br>' +
        (label ? '<small>' + label + '</small><br>' : '') +
        'لینک ارسال‌شده قدیمی است. لطفاً از Adobe Connect یک لینک جلسه جدید دریافت کنید.',
        'warn',
        0   // sticky — user must dismiss manually
      );
    }
  });

  evtSource.onerror = () => {
    setConnected(false);
    evtSource.close(); evtSource = null;
    clearTimeout(reconnectTimer);
    reconnectTimer = setTimeout(connectSSE, 3000);
  };
}

function setConnected(ok) {
  document.getElementById('connDot').classList.toggle('connected', ok);
  document.getElementById('connLabel').textContent = ok ? 'زنده' : 'در حال اتصال…';
}

// ─── Render ───────────────────────────────────────────────────────────────────
function render() {
  renderSlots();
  renderStats();
  renderActiveJobs();
  renderHistory();
}

function renderSlots() {
  const bar = document.getElementById('slotsBar');
  // Rebuild slot divs based on current maxConcurrent
  const existing = bar.querySelectorAll('.slot');
  if (existing.length !== maxConcurrent) {
    bar.innerHTML = '';
    for (let i = 0; i < maxConcurrent; i++) {
      const d = document.createElement('div');
      d.className = 'slot';
      d.id = 's' + i;
      bar.appendChild(d);
    }
  }
  bar.querySelectorAll('.slot').forEach((el, i) => {
    el.classList.toggle('active', i < running);
  });
  const queued = jobs.filter(j => j.status === 'queued').length;
  document.getElementById('slotsLabel').textContent =
    running + ' از ' + maxConcurrent + ' اسلات فعال' +
    (queued > 0 ? ' · ' + queued + ' در صف' : '');
}

function renderStats() {
  document.getElementById('statRunning').textContent = jobs.filter(j=>j.status==='running').length;
  document.getElementById('statQueued').textContent  = jobs.filter(j=>j.status==='queued').length;
  document.getElementById('statDone').textContent    = jobs.filter(j=>j.status==='done').length;
  document.getElementById('statFailed').textContent  = jobs.filter(j=>j.status==='failed').length;
}

function renderActiveJobs() {
  const active = jobs.filter(j => j.status === 'queued' || j.status === 'running');
  const area = document.getElementById('jobsArea');
  const noJobs = document.getElementById('noJobs');
  const existingIds = new Set(active.map(j => j.id));
  area.querySelectorAll('.job-card[data-id]').forEach(el => {
    if (!existingIds.has(el.dataset.id)) el.remove();
  });
  noJobs.style.display = active.length === 0 ? '' : 'none';
  active.forEach(job => {
    let card = area.querySelector('.job-card[data-id="' + job.id + '"]');
    if (!card) {
      card = buildJobCard(job);
      area.insertBefore(card, noJobs);
    } else {
      updateJobCard(card, job);
    }
  });
}

function buildJobCard(job) {
  const card = document.createElement('div');
  card.className = 'job-card ' + job.status;
  card.dataset.id = job.id;
  card.innerHTML = jobCardHTML(job);
  return card;
}

function updateJobCard(card, job) {
  card.className = 'job-card ' + job.status + (job.stalled ? ' stalled' : '');
  const badge = card.querySelector('.job-badge');
  if (badge) { badge.className = 'job-badge badge-' + job.status; badge.textContent = badgeLabel(job.status); }
  const dot = card.querySelector('.dot');
  if (dot) dot.className = 'dot dot-' + (job.stalled ? 'stalled' : job.status);
  updateProgress(job);
}

function jobCardHTML(job) {
  const logs = (job.log_lines || []).slice(-8);
  const logHTML = logs.length
    ? logs.map(l => '<div class="log-line' + (l.includes('ERROR') ? ' log-err' : l.includes('Progress') ? ' log-prog' : '') + '">' + escHtml(l) + '</div>').join('')
    : '<div class="log-empty">منتظر خروجی…</div>';

  const progressSection = job.status === 'running' ? progressHTML(job) : '';
  const testBadge = job.test_mode ? '<span class="badge-test">TEST</span>' : '';

  return ` + "`" + `
  <div class="job-header">
    <div class="dot dot-${job.status}"></div>
    <span class="job-badge badge-${job.status}">${badgeLabel(job.status)}</span>
    <span class="job-name" title="${escAttr(job.course_name)}">${testBadge}${escHtml(job.course_name)}</span>
    <span class="job-session">${escHtml(job.session || '')}</span>
  </div>
  <div class="job-meta">
    <span class="job-url" title="${escAttr(job.url)}">${escHtml(job.url)}</span>
    <span style="font-size:.72rem;color:var(--text3);white-space:nowrap">${fmtDate(job.created_at)}</span>
  </div>
  ${progressSection}
  <div class="log-area" id="log-${job.id}">${logHTML}</div>
  ` + "`" + `;
}

function progressHTML(job) {
  const elapsed = job.elapsed || '00:00:00';
  const total = job.total || '--:--:--';
  const pct = calcPct(elapsed, total);
  const stallHtml = job.stalled
    ? '<div class="stall-warn" id="sw-' + job.id + '">پیشرفتی در ۹۰ ثانیه اخیر مشاهده نشد</div>'
    : '<div class="stall-warn" id="sw-' + job.id + '" style="display:none">پیشرفتی در ۹۰ ثانیه اخیر مشاهده نشد</div>';
  return ` + "`" + `
  <div class="progress-wrap">
    <div class="progress-times"><span>${elapsed}</span><span>${total}</span></div>
    <div class="progress-bar"><div class="progress-fill" id="pf-${job.id}" style="width:${pct}%"></div></div>
  </div>${stallHtml}` + "`" + `;
}

function updateProgress(job) {
  const pf = document.getElementById('pf-' + job.id);
  if (!pf) return;
  pf.style.width = calcPct(job.elapsed || '00:00:00', job.total || '00:00:00') + '%';
  const wrap = pf.closest('.progress-wrap');
  if (wrap) {
    const spans = wrap.querySelectorAll('.progress-times span');
    if (spans[0]) spans[0].textContent = job.elapsed || '00:00:00';
    if (spans[1]) spans[1].textContent = job.total || '--:--:--';
  }
  // Show/hide stall warning
  const sw = document.getElementById('sw-' + job.id);
  if (sw) sw.style.display = job.stalled ? '' : 'none';
}

function appendLogLine(id, line) {
  const el = document.getElementById('log-' + id);
  if (!el) return;
  const div = document.createElement('div');
  div.className = 'log-line' + (line.includes('ERROR') ? ' log-err' : line.includes('Progress') ? ' log-prog' : '');
  div.textContent = line;
  el.appendChild(div);
  while (el.children.length > 200) el.removeChild(el.firstChild);
  el.querySelectorAll('.log-empty').forEach(e => e.remove());
  el.scrollTop = el.scrollHeight;
}

function renderHistory() {
  const done = jobs.filter(j => j.status === 'done' || j.status === 'failed');
  const tbody = document.getElementById('histBody');
  if (done.length === 0) {
    tbody.innerHTML = '<tr><td colspan="6" class="no-history">هنوز ضبطی انجام نشده.</td></tr>';
    return;
  }
  tbody.innerHTML = done.map(job => {
    const ts = Math.floor(Date.now() / 1000);
    const testBadge = job.test_mode ? '<span class="badge-test">TEST</span> ' : '';
    const dlCell = (job.status === 'done' && job.output_file)
      ? '<a class="btn-dl" href="/download/' + escAttr(job.output_file) + '?t=' + ts + '" download>&#11123; دانلود</a>'
      : (job.error ? '<span style="color:var(--red);font-size:.75rem">' + escHtml(job.error) + '</span>' : '—');
    const statusBadge = '<span class="job-badge badge-' + job.status + '">' + badgeLabel(job.status) + '</span>';
    const duration = (job.elapsed && job.total) ? job.elapsed + ' / ' + job.total : (job.total || '—');
    return '<tr><td>' + testBadge + escHtml(job.course_name) + '</td><td>' + escHtml(job.session||'—') +
      '</td><td>' + statusBadge + '</td><td class="td-date">' + fmtDate(job.finished_at||job.created_at) +
      '</td><td style="font-family:monospace;font-size:.8rem;text-align:right">' + duration +
      '</td><td>' + dlCell + '</td></tr>';
  }).join('');
}

// ─── Form ─────────────────────────────────────────────────────────────────────
document.getElementById('recForm').addEventListener('submit', async e => {
  e.preventDefault();
  const url      = document.getElementById('fUrl').value.trim();
  const course   = document.getElementById('fCourse').value.trim();
  const session  = document.getElementById('fSession').value.trim();
  const testMode = document.getElementById('fTest').checked;
  if (!url || !course) { toast('آدرس جلسه و نام درس را پر کنید.', 'warn'); return; }

  const btn = document.getElementById('submitBtn');
  const lbl = document.getElementById('submitLabel');
  const ico = document.getElementById('submitIcon');
  btn.disabled = true;
  ico.innerHTML = '<div class="spinner"></div>';
  lbl.textContent = 'در حال ارسال…';

  try {
    const res  = await fetch('/api/record', {
      method: 'POST',
      headers: {'Content-Type':'application/json'},
      body: JSON.stringify({url, course_name: course, session, test_mode: testMode})
    });
    const data = await res.json();
    if (!data.ok) { toast(data.message || 'خطا', 'error'); return; }
    if (data.duplicate) {
      if (data.in_progress) {
        toast('⚡ این آدرس در حال حاضر در صف ضبط است.', 'warn');
      } else if (data.download_url) {
        toast('✅ این جلسه قبلاً ضبط شده. <a href="' + data.download_url + '" download style="color:var(--accent)">دانلود</a>', 'success', 8000);
      } else {
        toast('ℹ️ ' + data.message, 'info');
      }
      return;
    }
    toast('ضبط در صف قرار گرفت.', 'success');
    document.getElementById('fUrl').value = '';
    document.getElementById('fSession').value = '';
  } catch(err) {
    toast('خطای شبکه: ' + err.message, 'error');
  } finally {
    btn.disabled = false;
    ico.textContent = '▶';
    lbl.textContent = 'شروع ضبط';
  }
});

// ─── Toast ────────────────────────────────────────────────────────────────────
function toast(msg, type = 'info', duration = 4000) {
  const icons = {info:'ℹ️', success:'✅', warn:'⚠️', error:'❌'};
  const area = document.getElementById('toastArea');
  const el = document.createElement('div');
  el.className = 'toast toast-' + type;
  el.innerHTML = '<span class="toast-icon">' + (icons[type]||'') + '</span><span style="flex:1">' + msg + '</span>' +
    '<button class="toast-close" onclick="this.parentElement.remove()">✕</button>';
  area.appendChild(el);
  if (duration > 0) setTimeout(() => el.remove(), duration);
}

// ─── Helpers ──────────────────────────────────────────────────────────────────
function badgeLabel(s) {
  return {queued:'در صف', running:'در حال ضبط', done:'انجام شد', failed:'خطا'}[s] || s;
}
function calcPct(elapsed, total) {
  const toSec = t => { const p = t.split(':').map(Number); return p[0]*3600+p[1]*60+p[2]; };
  const e = toSec(elapsed||'00:00:00'), t = toSec(total||'00:00:00');
  return t ? Math.min(100, Math.round(e/t*1000)/10) : 0;
}
function fmtDate(iso) {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  return d.toLocaleDateString('fa-IR') + ' ' + d.toLocaleTimeString('fa-IR', {hour:'2-digit', minute:'2-digit'});
}
function escHtml(s) {
  return String(s||'').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}
function escAttr(s) { return escHtml(s); }

// ─── File browser ─────────────────────────────────────────────────────────────
let allFiles = [];

// Wrap matching text in a highlight span.
function highlight(text, q) {
  const s = escHtml(text || '');
  if (!q) return s;
  const idx = s.toLowerCase().indexOf(q.toLowerCase());
  if (idx < 0) return s;
  return s.slice(0, idx) +
    '<mark style="background:rgba(88,166,255,.3);color:var(--text);border-radius:2px;padding:0 1px">' +
    s.slice(idx, idx + q.length) + '</mark>' + s.slice(idx + q.length);
}

async function loadFiles() {
  document.getElementById('fileCount').textContent = '…';
  try {
    const res = await fetch('/api/files');
    const data = await res.json();
    allFiles = data.files || [];
    filterFiles();
  } catch(e) {
    document.getElementById('fileCount').textContent = 'خطا';
  }
}

function filterFiles() {
  const q = (document.getElementById('fileSearch')?.value || '').trim().toLowerCase();

  const filtered = q
    ? allFiles.filter(f =>
        (f.name||'').toLowerCase().includes(q) ||
        (f.course_name||'').toLowerCase().includes(q) ||
        (f.session||'').toLowerCase().includes(q)
      )
    : allFiles;

  const tbody = document.getElementById('filesBody');
  const countEl = document.getElementById('fileCount');
  if (countEl) countEl.textContent = filtered.length + ' فایل';

  if (!tbody) return;
  if (filtered.length === 0) {
    tbody.innerHTML = '<tr><td colspan="6" class="no-files">' +
      (q ? 'نتیجه‌ای برای «' + escHtml(q) + '» یافت نشد.' : 'هیچ فایلی در پوشه ضبط‌ها موجود نیست.') +
      '</td></tr>';
    return;
  }
  const ts = Math.floor(Date.now() / 1000);
  tbody.innerHTML = filtered.map(f => {
    const courseHTML = q ? highlight(f.course_name||'—', q) : escHtml(f.course_name||'—');
    const sessionHTML = q ? highlight(f.session||'—', q) : escHtml(f.session||'—');
    const nameHTML = q ? highlight(f.name, q) : escHtml(f.name);
    const dlUrl = '/download/' + escAttr(f.name) + '?t=' + ts;
    const noMeta = !f.course_name;
    return '<tr>' +
      '<td class="td-course" title="' + escAttr(f.course_name||'') + '">' +
        (noMeta ? '<span style="color:var(--text3)">—</span>' : courseHTML) + '</td>' +
      '<td class="td-session">' + (noMeta ? '<span style="color:var(--text3)">—</span>' : sessionHTML) + '</td>' +
      '<td class="td-name"><a href="' + dlUrl + '" download>' + nameHTML + '</a></td>' +
      '<td class="td-size">' + fmtSize(f.size) + '</td>' +
      '<td class="td-date">' + fmtDate(f.modified_at) + '</td>' +
      '<td><a class="btn-dl" href="' + dlUrl + '" download>&#11123; دانلود</a></td>' +
      '</tr>';
  }).join('');
}

function fmtSize(bytes) {
  if (!bytes) return '—';
  if (bytes >= 1073741824) return (bytes/1073741824).toFixed(1) + ' GB';
  if (bytes >= 1048576) return (bytes/1048576).toFixed(1) + ' MB';
  if (bytes >= 1024) return (bytes/1024).toFixed(0) + ' KB';
  return bytes + ' B';
}

loadFiles();
setInterval(loadFiles, 30000);

connectSSE();
</script>
</body>
</html>`
