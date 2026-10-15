package transcribe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// The test binary doubles as the wrapper: with TRANSCRIBE_FAKE_WRAPPER=1 it
// follows the real wrapper's contract, choosing an outcome by the audio's
// content: "fail" exits 1 with error.txt, "exit2" exits 2 without it, "sleep"
// hangs, anything else writes the three transcripts and exits 0.
func TestMain(m *testing.M) {
	if os.Getenv("TRANSCRIBE_FAKE_WRAPPER") == "1" {
		os.Exit(fakeWrapper(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeWrapper(args []string) int {
	opt := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		opt[args[i]] = args[i+1]
	}
	out := opt["-OutDir"]
	if _, err := os.Stat(out); err != nil {
		return 2
	}
	audio, _ := os.ReadFile(opt["-InputPath"])
	fmt.Printf("[   0.1s] transcribing speakers=%s language=%s\n", opt["-Speakers"], opt["-Language"])
	write := func(name, text string) { _ = os.WriteFile(filepath.Join(out, name), []byte(text), 0o600) }
	switch string(audio) {
	case "fail":
		write("error.txt", "RuntimeError: CUDA out of memory\n")
		fmt.Println("elapsed_seconds=0 exit=1")
		return 1
	case "exit2":
		return 2
	case "sleep":
		time.Sleep(time.Minute)
	}
	write("transcript.json", `{"segments":[{"start":0.5,"end":2,"speaker":"SPEAKER_00","text":"Hello there.","words":[]},{"start":2.5,"end":4,"speaker":"SPEAKER_01","text":"Hi.","words":[]}],"language":"en"}`)
	write("transcript.srt", "1\n00:00:00,500 --> 00:00:02,000\n[SPEAKER_00]: Hello there.\n")
	write("transcript.txt", "[00:00:00] SPEAKER_00: Hello there.\n[00:00:02] SPEAKER_01: Hi.\n")
	fmt.Println("elapsed_seconds=0 exit=0")
	return 0
}

func newService(t *testing.T, cfg config.Transcription) (*Service, *gin.Engine) {
	t.Helper()
	t.Setenv("TRANSCRIBE_FAKE_WRAPPER", "1")
	gin.SetMode(gin.TestMode)
	cfg.Enabled, cfg.Command = true, os.Args[0]
	if cfg.Dir == "" {
		cfg.Dir = t.TempDir()
	}
	s := New(t.TempDir())
	s.Configure(cfg)
	t.Cleanup(s.Close)
	r := gin.New()
	g := r.Group("/t")
	g.GET("", s.List)
	g.POST("", s.Upload)
	g.GET("/:id", s.Get)
	g.GET("/:id/:format", s.Download)
	g.DELETE("/:id", s.Delete)
	return s, r
}

func do(r http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// upload posts a form; fields are name, value pairs, and a "file" pair's
// value is "filename=content".
func upload(r http.Handler, fields ...string) *httptest.ResponseRecorder {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "file" {
			name, content, _ := strings.Cut(fields[i+1], "=")
			fw, _ := w.CreateFormFile("file", name)
			_, _ = fw.Write([]byte(content))
			continue
		}
		_ = w.WriteField(fields[i], fields[i+1])
	}
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/t", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func submit(t *testing.T, r http.Handler, fields ...string) string {
	t.Helper()
	rec := upload(r, fields...)
	var reply struct{ ID string }
	if rec.Code != http.StatusAccepted || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || reply.ID == "" {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	return reply.ID
}

func waitFor(t *testing.T, r http.Handler, id, status string) jobReply {
	t.Helper()
	var job jobReply
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		rec := do(r, http.MethodGet, "/t/"+id)
		if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", id, rec.Code, rec.Body)
		}
		if job.Status == status {
			return job
		}
	}
	t.Fatalf("job %s is %s (%s), want %s", id, job.Status, job.Error, status)
	return job
}

func TestUploadQueueDoneDownloadDelete(t *testing.T) {
	_, r := newService(t, config.Transcription{})
	first := submit(t, r, "speakers", "2", "language", "EN", "file", "memo one.m4a=hello")
	second := submit(t, r, "file", "memo two.mp3=hello")

	a, b := waitFor(t, r, first, Done), waitFor(t, r, second, Done)
	if a.Name != "memo one.m4a" || a.Size != 5 || a.Speakers != 2 || a.Language != "en" || !strings.Contains(string(a.Transcript), "SPEAKER_01") {
		t.Fatalf("first job: %+v", a)
	}
	if b.Started.Before(*a.Finished) {
		t.Fatalf("jobs overlapped: second started %v before first finished %v", b.Started, a.Finished)
	}
	var list struct{ Jobs []Meta }
	if rec := do(r, http.MethodGet, "/t"); json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Jobs) != 2 {
		t.Fatalf("list: %s", rec.Body)
	}
	for format, want := range map[string]string{"txt": "SPEAKER_01: Hi.", "srt": "[SPEAKER_00]: Hello there.", "json": `"speaker":"SPEAKER_00"`} {
		rec := do(r, http.MethodGet, "/t/"+first+"/"+format)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) || !strings.Contains(rec.Header().Get("Content-Disposition"), "memo one."+format) {
			t.Fatalf("%s: %d %q %s", format, rec.Code, rec.Header().Get("Content-Disposition"), rec.Body)
		}
	}
	if rec := do(r, http.MethodDelete, "/t/"+first); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec := do(r, http.MethodGet, "/t/"+first); rec.Code != http.StatusNotFound {
		t.Fatalf("after delete: %d", rec.Code)
	}
}

func TestFailures(t *testing.T) {
	s, r := newService(t, config.Transcription{Timeout: "300ms"})
	if job := waitFor(t, r, submit(t, r, "file", "a.wav=fail"), Failed); job.Error != "RuntimeError: CUDA out of memory" {
		t.Fatalf("exit 1: %q, want error.txt", job.Error)
	}
	if job := waitFor(t, r, submit(t, r, "file", "b.wav=exit2"), Failed); !strings.Contains(job.Error, "exit 2") {
		t.Fatalf("exit 2: %q", job.Error)
	}
	if job := waitFor(t, r, submit(t, r, "file", "c.wav=sleep"), Failed); job.Error != "timed out after 300ms" {
		t.Fatalf("timeout: %q", job.Error)
	}

	// A running job cannot be deleted; shutdown kills it and it fails as interrupted.
	s.Configure(config.Transcription{Enabled: true, Command: os.Args[0], Dir: s.dir, Timeout: "1m"})
	id := submit(t, r, "file", "d.wav=sleep")
	waitFor(t, r, id, Running)
	if rec := do(r, http.MethodDelete, "/t/"+id); rec.Code != http.StatusConflict {
		t.Fatalf("delete while running: %d", rec.Code)
	}
	start := time.Now()
	s.Close()
	if time.Since(start) > 5*time.Second {
		t.Fatalf("close took %s", time.Since(start))
	}
	if m, _ := s.readMeta(filepath.Join(s.dir, id)); m.Status != Failed || m.Error != "interrupted" {
		t.Fatalf("after shutdown: %+v", m)
	}
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	jobs := map[string]Meta{
		"20260101-000000-00000001": {Status: Running, Name: "was-running.m4a"},
		"20260101-000000-00000002": {Status: Queued, Name: "queued.m4a"},
		"20260101-000000-00000003": {Status: Done, Name: "done.m4a"},
	}
	for id, m := range jobs {
		m.ID, m.Created = id, time.Now().UTC()
		_ = os.Mkdir(filepath.Join(dir, id), 0o700)
		_ = os.WriteFile(filepath.Join(dir, id, "audio.m4a"), []byte("hello"), 0o600)
		if err := s.writeMeta(filepath.Join(dir, id), m); err != nil {
			t.Fatal(err)
		}
	}
	_, r := newService(t, config.Transcription{Dir: dir})
	if job := waitFor(t, r, "20260101-000000-00000001", Failed); job.Error != "interrupted" {
		t.Fatalf("running job after restart: %+v", job)
	}
	waitFor(t, r, "20260101-000000-00000002", Done)
	if job := waitFor(t, r, "20260101-000000-00000003", Done); job.Started != nil {
		t.Fatalf("a done job ran again: %+v", job)
	}
}

func TestUploadRejects(t *testing.T) {
	_, r := newService(t, config.Transcription{MaxUploadMB: 1})
	cases := []struct {
		name   string
		fields []string
		want   int
	}{
		{"not audio", []string{"file", "evil.exe=hello"}, http.StatusBadRequest},
		{"no file", []string{"speakers", "2"}, http.StatusBadRequest},
		{"empty", []string{"file", "a.m4a="}, http.StatusBadRequest},
		{"too big", []string{"file", "a.m4a=" + strings.Repeat("x", 1<<20+1)}, http.StatusRequestEntityTooLarge},
		{"two files", []string{"file", "a.m4a=x", "file", "b.m4a=x"}, http.StatusBadRequest},
		{"speakers", []string{"speakers", "0", "file", "a.m4a=x"}, http.StatusBadRequest},
		{"language", []string{"language", "english", "file", "a.m4a=x"}, http.StatusBadRequest},
	}
	if runtime.GOOS != "windows" { // Windows reduces this name to its base first.
		cases = append(cases, struct {
			name   string
			fields []string
			want   int
		}{"traversal", []string{"file", `..\..\evil.m4a=x`}, http.StatusBadRequest})
	}
	for _, tc := range cases {
		if rec := upload(r, tc.fields...); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, rec.Code, rec.Body, tc.want)
		}
	}
	var list struct{ Jobs []Meta }
	if rec := do(r, http.MethodGet, "/t"); json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Jobs) != 0 {
		t.Fatalf("a refused upload left a job: %s", rec.Body)
	}
	for _, path := range []string{"/t/..", "/t/x", "/t/20260101-000000-00000009/txt"} {
		if rec := do(r, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", path, rec.Code)
		}
	}

	off := New(t.TempDir())
	off.Configure(config.Transcription{})
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	if off.List(c); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: %d, want 404", rec.Code)
	}
}
