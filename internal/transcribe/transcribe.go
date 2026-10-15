// Package transcribe turns uploaded voice memos into speaker-labelled
// transcripts. A wrapper command does the speech work on the GPU; this
// package keeps the jobs, one folder each, and runs them one at a time.
package transcribe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// Job statuses, as stored in meta.json.
const (
	Queued  = "queued"
	Running = "running"
	Done    = "done"
	Failed  = "failed"
)

const (
	defaultMaxUploadMB = 500
	defaultTimeout     = 2 * time.Hour
	maxSpeakers        = 20
)

var (
	audioExts = []string{".m4a", ".mp3", ".wav", ".ogg", ".opus", ".webm", ".mp4", ".aac", ".flac"}
	// Job ids are made here, so anything else in a path is refused.
	idPattern       = regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{8}$`)
	languagePattern = regexp.MustCompile(`^[a-z]{2,3}$`)
	outputs         = map[string]string{"txt": "transcript.txt", "srt": "transcript.srt", "json": "transcript.json"}
)

// Meta is a job's meta.json.
type Meta struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Size     int64      `json:"size"`
	Created  time.Time  `json:"created"`
	Status   string     `json:"status"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Speakers int        `json:"speakers,omitempty"`
	Language string     `json:"language,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// Service holds the job queue and runs it on one worker.
type Service struct {
	defaultDir string

	mu      sync.Mutex
	cond    *sync.Cond
	cfg     config.Transcription
	dir     string   // absolute jobs folder
	queue   []string // job folders, oldest first
	running string   // job folder being run
	started bool
	closed  bool

	// fileMu orders meta.json reads and writes: Windows cannot replace a
	// file another goroutine has open.
	fileMu sync.Mutex

	ctx  context.Context
	stop context.CancelFunc
	done chan struct{}
}

// New makes a service that stays idle until Configure enables it. Jobs go to
// defaultDir unless the config names another folder.
func New(defaultDir string) *Service {
	s := &Service{defaultDir: defaultDir, done: make(chan struct{})}
	s.cond = sync.NewCond(&s.mu)
	s.ctx, s.stop = context.WithCancel(context.Background())
	return s
}

// Configure applies the transcription config, at start and on each reload.
// Turning it on, or pointing it at another folder, rescans that folder:
// queued jobs queue again and jobs left running by a restart fail as
// "interrupted". Turning it off lets a running job finish but starts no
// more until it is on again.
func (s *Service) Configure(cfg config.Transcription) {
	dir := cfg.Dir
	if dir == "" {
		dir = s.defaultDir
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	rescan := cfg.Enabled && (!s.cfg.Enabled || dir != s.dir)
	s.cfg, s.dir = cfg, dir
	if !cfg.Enabled {
		return
	}
	if rescan {
		s.queue = s.recover(dir)
	}
	if !s.started {
		s.started = true
		go s.loop()
	}
	s.cond.Broadcast()
}

// recover reads dir after a restart or a config change. Called with mu held.
func (s *Service) recover(dir string) []string {
	var queued []Meta
	for _, m := range s.scan(dir) {
		jobDir := filepath.Join(dir, m.ID)
		switch {
		case m.Status == Queued:
			queued = append(queued, m)
		case m.Status == Running && jobDir != s.running:
			now := time.Now().UTC()
			m.Status, m.Finished, m.Error = Failed, &now, "interrupted"
			if err := s.writeMeta(jobDir, m); err != nil {
				log.Warnf("transcription %s: %v", m.ID, err)
			}
		}
	}
	slices.SortFunc(queued, func(a, b Meta) int { return a.Created.Compare(b.Created) })
	out := make([]string, len(queued))
	for i, m := range queued {
		out[i] = filepath.Join(dir, m.ID)
	}
	return out
}

// Close kills a running job, which then fails as "interrupted", and waits
// for the worker to stop.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	started := s.started
	s.cond.Broadcast()
	s.mu.Unlock()
	s.stop()
	if started {
		<-s.done
	}
}

func (s *Service) loop() {
	defer close(s.done)
	for {
		s.mu.Lock()
		for !s.closed && (!s.cfg.Enabled || len(s.queue) == 0) {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		jobDir := s.queue[0]
		s.queue = s.queue[1:]
		s.running = jobDir
		cfg := s.cfg
		s.mu.Unlock()

		s.run(jobDir, cfg)

		s.mu.Lock()
		s.running = ""
		s.mu.Unlock()
	}
}

func (s *Service) run(jobDir string, cfg config.Transcription) {
	m, err := s.readMeta(jobDir)
	if err != nil {
		log.Warnf("transcription: skip %s: %v", jobDir, err)
		return
	}
	now := time.Now().UTC()
	m.Status, m.Started = Running, &now
	if err = s.writeMeta(jobDir, m); err != nil {
		log.Warnf("transcription %s: %v", m.ID, err)
	}
	m.Error = s.execute(jobDir, m, cfg)
	end := time.Now().UTC()
	m.Status, m.Finished = Done, &end
	if m.Error != "" {
		m.Status = Failed
	}
	if err = s.writeMeta(jobDir, m); err != nil {
		log.Warnf("transcription %s: %v", m.ID, err)
	}
}

// execute runs the wrapper and returns why the job failed, or "".
func (s *Service) execute(jobDir string, m Meta, cfg config.Transcription) string {
	if cfg.Command == "" {
		return "transcription.command is not set in the proxy's config"
	}
	timeout := defaultTimeout
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			timeout = d
		} else {
			log.Warnf("transcription.timeout %q is not a duration; using %s", cfg.Timeout, timeout)
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, timeout)
	defer cancel()

	out, err := os.Create(filepath.Join(jobDir, "stdout.log"))
	if err != nil {
		return "could not create stdout.log: " + err.Error()
	}
	defer func() { _ = out.Close() }()
	args := append(slices.Clone(cfg.Args), "-InputPath", filepath.Join(jobDir, "audio"+audioExt(m.Name)), "-OutDir", jobDir)
	if m.Speakers > 0 {
		args = append(args, "-Speakers", strconv.Itoa(m.Speakers))
	}
	if m.Language != "" {
		args = append(args, "-Language", m.Language)
	}
	cmd := exec.CommandContext(ctx, cfg.Command, args...)
	cmd.Dir = jobDir
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = 10 * time.Second
	killTreeOnCancel(cmd)
	err = cmd.Run()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timed out after " + timeout.String()
	case ctx.Err() != nil:
		return "interrupted"
	case err != nil:
		if why := readHead(filepath.Join(jobDir, "error.txt"), 4096); why != "" {
			return why
		}
		// The wrapper exits 2, without error.txt, when the job folder is missing.
		if exit, isExit := errors.AsType[*exec.ExitError](err); isExit && exit.ExitCode() == 2 {
			return "internal error: the wrapper did not find the job folder (exit 2)"
		}
		return "the wrapper failed: " + err.Error()
	}
	for _, name := range []string{"transcript.json", "transcript.srt", "transcript.txt"} {
		if _, errStat := os.Stat(filepath.Join(jobDir, name)); errStat != nil {
			return "the wrapper exited 0 without writing " + name
		}
	}
	return ""
}

// ---------- files ----------

func (s *Service) readMeta(jobDir string) (Meta, error) {
	s.fileMu.Lock()
	data, err := os.ReadFile(filepath.Join(jobDir, "meta.json"))
	s.fileMu.Unlock()
	var m Meta
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	return m, err
}

// writeMeta replaces meta.json whole, so a crash never leaves half of one.
func (s *Service) writeMeta(jobDir string, m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	tmp := filepath.Join(jobDir, "meta.json.tmp")
	if err = os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(jobDir, "meta.json"))
}

// scan lists the jobs in dir, newest first.
func (s *Service) scan(dir string) []Meta {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warnf("transcription: read %s: %v", dir, err)
	}
	jobs := []Meta{}
	for _, e := range entries {
		if !e.IsDir() || !idPattern.MatchString(e.Name()) {
			continue
		}
		if m, errRead := s.readMeta(filepath.Join(dir, e.Name())); errRead == nil && m.ID == e.Name() {
			jobs = append(jobs, m)
		}
	}
	slices.SortFunc(jobs, func(a, b Meta) int { return b.Created.Compare(a.Created) })
	return jobs
}

// readHead returns up to max bytes of a file, trimmed; "" when unreadable.
func readHead(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	data, _ := io.ReadAll(io.LimitReader(f, max))
	return strings.TrimSpace(strings.ToValidUTF8(string(data), ""))
}

// lastLine is the wrapper's latest progress line, from the end of stdout.log.
func lastLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if info, errStat := f.Stat(); errStat == nil && info.Size() > 2048 {
		_, _ = f.Seek(-2048, io.SeekEnd)
	}
	data, _ := io.ReadAll(f)
	lines := strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(strings.ToValidUTF8(lines[i], "")); line != "" {
			return line
		}
	}
	return ""
}

func audioExt(name string) string { return strings.ToLower(filepath.Ext(name)) }

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b)
}

// checkName accepts a plain file name with an audio extension. The name is
// only shown back; the audio is stored as audio.<ext>.
func checkName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || !utf8.ValidString(name) ||
		strings.ContainsAny(name, `/\:`) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("the file needs a plain file name")
	}
	if !slices.Contains(audioExts, audioExt(name)) {
		return fmt.Errorf("only %s files can be transcribed", strings.ReplaceAll(strings.Join(audioExts, ", "), ".", ""))
	}
	return nil
}

// ---------- HTTP ----------

// settings reads what a request needs, and refuses when transcription is off.
func (s *Service) settings(c *gin.Context) (config.Transcription, string, bool) {
	s.mu.Lock()
	cfg, dir, closed := s.cfg, s.dir, s.closed
	s.mu.Unlock()
	if !cfg.Enabled || closed {
		c.JSON(http.StatusNotFound, gin.H{"error": "transcription is disabled; set transcription.enabled in the proxy's config"})
		return cfg, "", false
	}
	return cfg, dir, true
}

// jobDir resolves the :id parameter, or answers 404.
func (s *Service) jobDir(c *gin.Context) (string, bool) {
	_, dir, ok := s.settings(c)
	if !ok {
		return "", false
	}
	id := c.Param("id")
	if !idPattern.MatchString(id) {
		c.JSON(http.StatusNotFound, gin.H{"error": "no such transcription"})
		return "", false
	}
	return filepath.Join(dir, id), true
}

// Upload takes a multipart form: file, and optional speakers and language.
// It stores the job and queues it: 202 {"id"}.
func (s *Service) Upload(c *gin.Context) {
	cfg, dir, ok := s.settings(c)
	if !ok {
		return
	}
	if cfg.Command == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "transcription.command is not set in the proxy's config"})
		return
	}
	maxMB := cfg.MaxUploadMB
	if maxMB <= 0 {
		maxMB = defaultMaxUploadMB
	}
	limit := int64(maxMB) << 20
	// The form's headers and small fields ride on top of the file.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit+1<<20)
	reader, err := c.Request.MultipartReader()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "send a multipart form with the audio in a field named file"})
		return
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create the transcriptions folder: " + err.Error()})
		return
	}
	m := Meta{ID: newID(), Created: time.Now().UTC(), Status: Queued}
	jobDir := filepath.Join(dir, m.ID)
	if err = os.Mkdir(jobDir, 0o700); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create the job folder: " + err.Error()})
		return
	}
	kept := false
	defer func() {
		if !kept {
			_ = os.RemoveAll(jobDir)
		}
	}()
	tooBig := fmt.Sprintf("the file is larger than %d MB", maxMB)
	fail := func(err error, status int, msg string) {
		var big *http.MaxBytesError
		if errors.As(err, &big) {
			status, msg = http.StatusRequestEntityTooLarge, tooBig
		}
		c.JSON(status, gin.H{"error": msg})
	}
	for {
		part, errPart := reader.NextPart()
		if errors.Is(errPart, io.EOF) {
			break
		}
		if errPart != nil {
			fail(errPart, http.StatusBadRequest, "the form could not be read")
			return
		}
		switch part.FormName() {
		case "file":
			if m.Name != "" {
				fail(nil, http.StatusBadRequest, "send one file per upload")
				return
			}
			name := strings.TrimSpace(part.FileName())
			if errName := checkName(name); errName != nil {
				fail(nil, http.StatusBadRequest, errName.Error())
				return
			}
			f, errCreate := os.OpenFile(filepath.Join(jobDir, "audio"+audioExt(name)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if errCreate != nil {
				fail(nil, http.StatusInternalServerError, "could not store the audio: "+errCreate.Error())
				return
			}
			n, errCopy := io.Copy(f, io.LimitReader(part, limit+1))
			if errClose := f.Close(); errCopy == nil {
				errCopy = errClose
			}
			switch {
			case errCopy != nil:
				fail(errCopy, http.StatusBadRequest, "the upload broke off: "+errCopy.Error())
				return
			case n > limit:
				fail(nil, http.StatusRequestEntityTooLarge, tooBig)
				return
			case n == 0:
				fail(nil, http.StatusBadRequest, "the file is empty")
				return
			}
			m.Name, m.Size = name, n
		case "speakers":
			v := field(part)
			if v == "" {
				continue
			}
			n, errAtoi := strconv.Atoi(v)
			if errAtoi != nil || n < 1 || n > maxSpeakers {
				fail(nil, http.StatusBadRequest, fmt.Sprintf("speakers must be a whole number from 1 to %d", maxSpeakers))
				return
			}
			m.Speakers = n
		case "language":
			v := strings.ToLower(field(part))
			if v != "" && !languagePattern.MatchString(v) {
				fail(nil, http.StatusBadRequest, "language must be a two- or three-letter code such as en")
				return
			}
			m.Language = v
		}
	}
	if m.Name == "" {
		fail(nil, http.StatusBadRequest, "send the audio in a field named file")
		return
	}
	if err = s.writeMeta(jobDir, m); err != nil {
		fail(nil, http.StatusInternalServerError, "could not save the job: "+err.Error())
		return
	}
	s.mu.Lock()
	if s.dir == dir {
		s.queue = append(s.queue, jobDir)
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	kept = true
	c.JSON(http.StatusAccepted, gin.H{"id": m.ID})
}

// field reads a small form value.
func field(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 64))
	return strings.TrimSpace(string(data))
}

// List answers {"jobs": [meta...]}, newest first.
func (s *Service) List(c *gin.Context) {
	if _, dir, ok := s.settings(c); ok {
		c.JSON(http.StatusOK, gin.H{"jobs": s.scan(dir)})
	}
}

type jobReply struct {
	Meta
	// Progress is the wrapper's latest stdout line while the job runs.
	Progress   string          `json:"progress,omitempty"`
	Transcript json.RawMessage `json:"transcript,omitempty"`
}

// Get answers a job's meta, with the transcript JSON once it is done.
func (s *Service) Get(c *gin.Context) {
	jobDir, ok := s.jobDir(c)
	if !ok {
		return
	}
	m, err := s.readMeta(jobDir)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no such transcription"})
		return
	}
	reply := jobReply{Meta: m}
	switch m.Status {
	case Running:
		reply.Progress = lastLine(filepath.Join(jobDir, "stdout.log"))
	case Done:
		if data, errRead := os.ReadFile(filepath.Join(jobDir, "transcript.json")); errRead == nil && json.Valid(data) {
			reply.Transcript = data
		}
	}
	c.JSON(http.StatusOK, reply)
}

// Download sends one of a done job's transcripts: txt, srt or json.
func (s *Service) Download(c *gin.Context) {
	jobDir, ok := s.jobDir(c)
	if !ok {
		return
	}
	file, known := outputs[c.Param("format")]
	m, err := s.readMeta(jobDir)
	if !known || err != nil || m.Status != Done {
		c.JSON(http.StatusNotFound, gin.H{"error": "no such transcript; formats are txt, srt and json once the job is done"})
		return
	}
	c.FileAttachment(filepath.Join(jobDir, file), strings.TrimSuffix(m.Name, filepath.Ext(m.Name))+"."+c.Param("format"))
}

// Delete removes a job's folder. A running job cannot be deleted.
func (s *Service) Delete(c *gin.Context) {
	jobDir, ok := s.jobDir(c)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == jobDir {
		c.JSON(http.StatusConflict, gin.H{"error": "the job is running; delete it once it has finished"})
		return
	}
	if _, err := os.Stat(filepath.Join(jobDir, "meta.json")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no such transcription"})
		return
	}
	s.queue = slices.DeleteFunc(s.queue, func(d string) bool { return d == jobDir })
	if err := os.RemoveAll(jobDir); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not delete it: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": filepath.Base(jobDir)})
}
