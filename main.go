//go:build windows

// gr: a small third-party Gensokyo Radio client for Windows 10.
// It plays the stream with mpv, polls the station API, scrobbles to Last.fm
// and draws a TUI with raw ANSI sequences (no TUI library, to keep memory low).
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	streamURL  = "https://stream.gensokyoradio.net/3/"
	apiURL     = "https://gensokyoradio.net/api/station/playing/"
	lastfmURL  = "https://ws.audioscrobbler.com/2.0/"
	streamKey  = "3"
	userAgent  = "gr-tui/1.0"
	maxScrobTh = 60 // seconds
)

// ---------------------------------------------------------------- API types

type songInfo struct {
	Title  string `json:"TITLE"`
	Artist string `json:"ARTIST"`
	Album  string `json:"ALBUM"`
	Year   string `json:"YEAR"`
	Circle string `json:"CIRCLE"`
}

type apiResp struct {
	ServerInfo struct {
		Listeners int `json:"LISTENERS"`
		Streams   map[string]struct {
			Listeners int `json:"LISTENERS"`
		} `json:"STREAMS"`
	} `json:"SERVERINFO"`
	SongInfo  songInfo `json:"SONGINFO"`
	SongTimes struct {
		Duration  int   `json:"DURATION"`
		Played    int   `json:"PLAYED"`
		Remaining int   `json:"REMAINING"`
		SongStart int64 `json:"SONGSTART"`
	} `json:"SONGTIMES"`
	SongData struct {
		Rating     string `json:"RATING"`
		TimesRated int    `json:"TIMESRATED"`
	} `json:"SONGDATA"`
}

// ---------------------------------------------------------------- shared state

type snap struct {
	ok       bool
	r        apiResp
	fetched  time.Time
	apiErr   string
	mpvSince time.Time
	mpvAccum time.Duration
	mpv      string
	lfm      string
}

func (s *snap) playedNow() float64 {
	p := float64(s.r.SongTimes.Played) + time.Since(s.fetched).Seconds()
	if d := float64(s.r.SongTimes.Duration); d > 0 && p > d {
		p = d
	}
	return p
}

func (s *snap) listened() time.Duration {
	d := s.mpvAccum
	if !s.mpvSince.IsZero() {
		d += time.Since(s.mpvSince)
	}
	return d
}

type state struct {
	mu sync.Mutex
	s  snap
}

func (st *state) get() snap {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

func (st *state) with(f func(*snap)) {
	st.mu.Lock()
	f(&st.s)
	st.mu.Unlock()
}

// ---------------------------------------------------------------- config

type config struct {
	APIKey     string `json:"api_key"`
	APISecret  string `json:"api_secret"`
	SessionKey string `json:"session_key"`
}

func configPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "gr-tui", "config.json")
}

func loadConfig(path string) config {
	var c config
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(path string, c config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// ---------------------------------------------------------------- HTTP

func newClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     30 * time.Second,
			TLSHandshakeTimeout: 8 * time.Second,
			ReadBufferSize:      4096,
			WriteBufferSize:     4096,
			// A non-nil empty map disables HTTP/2, which saves memory.
			TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
}

func fetch(ctx context.Context, c *http.Client) (*apiResp, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API HTTP %d", resp.StatusCode)
	}
	var r apiResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func clean(si *songInfo) {
	strip := func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}
	for _, p := range []*string{&si.Title, &si.Artist, &si.Album, &si.Year, &si.Circle} {
		*p = strings.Map(strip, *p)
	}
}

func poll(ctx context.Context, st *state, c *http.Client) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		next := 10 * time.Second
		r, err := fetch(ctx, c)
		if err != nil {
			msg := err.Error()
			st.with(func(s *snap) { s.apiErr = msg })
			next = 5 * time.Second
		} else {
			clean(&r.SongInfo)
			now := time.Now()
			st.with(func(s *snap) {
				s.ok = true
				s.r = *r
				s.fetched = now
				s.apiErr = ""
			})
			switch rem := r.SongTimes.Remaining; {
			case rem <= 2:
				next = 2 * time.Second
			case rem < 10:
				next = time.Duration(rem+1) * time.Second
			}
		}
		timer.Reset(next)
	}
}

// ---------------------------------------------------------------- mpv

func newKillJob() windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	if err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	return job // never closed: Windows closes it when this process dies and kills mpv
}

func assignJob(job windows.Handle, pid int) {
	if job == 0 {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(job, h)
}

var mpvArgs = []string{
	"--no-video",
	"--no-terminal",
	"--force-window=no",
	"--audio-display=no",
	"--idle=no",
	"--title=Gensokyo Radio",
	streamURL,
}

// runMPV keeps exactly one mpv process alive. It starts a new one only after
// the previous one has exited, so the station never sees two connections.
func runMPV(ctx context.Context, st *state, job windows.Handle) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "mpv", mpvArgs...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
		if err := cmd.Start(); err != nil {
			msg := "mpv: cannot start (" + err.Error() + ")"
			st.with(func(s *snap) { s.mpv = msg })
		} else {
			assignJob(job, cmd.Process.Pid)
			st.with(func(s *snap) { s.mpv = "mpv: playing"; s.mpvSince = time.Now() })
			_ = cmd.Wait()
			st.with(func(s *snap) {
				if !s.mpvSince.IsZero() {
					s.mpvAccum += time.Since(s.mpvSince)
					s.mpvSince = time.Time{}
				}
				s.mpv = "mpv: exited, restarting"
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// ---------------------------------------------------------------- Last.fm

type lastfm struct {
	c   *http.Client
	cfg config
}

type lfmResp struct {
	Error   int    `json:"error"`
	Message string `json:"message"`
	Token   string `json:"token"`
	Session struct {
		Key string `json:"key"`
	} `json:"session"`
}

func (l *lastfm) sign(p url.Values) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		if k == "format" || k == "callback" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(p.Get(k))
	}
	b.WriteString(l.cfg.APISecret)
	sum := md5.Sum([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (l *lastfm) call(ctx context.Context, method string, p url.Values, withSession bool) (*lfmResp, error) {
	p.Set("method", method)
	p.Set("api_key", l.cfg.APIKey)
	if withSession {
		p.Set("sk", l.cfg.SessionKey)
	}
	p.Set("api_sig", l.sign(p))
	p.Set("format", "json")

	var req *http.Request
	var err error
	if strings.HasPrefix(method, "auth.") {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, lastfmURL+"?"+p.Encode(), nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, lastfmURL, strings.NewReader(p.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := l.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var r lfmResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return nil, fmt.Errorf("Last.fm HTTP %d: %w", resp.StatusCode, err)
	}
	if r.Error != 0 {
		return nil, fmt.Errorf("Last.fm error %d: %s", r.Error, r.Message)
	}
	return &r, nil
}

type track struct {
	artist, title, album string
	start                int64
	dur                  int
}

func (l *lastfm) send(ctx context.Context, method string, t track, scrobble bool) error {
	p := url.Values{}
	p.Set("artist", t.artist)
	p.Set("track", t.title)
	if t.album != "" {
		p.Set("album", t.album)
	}
	if t.dur > 0 {
		p.Set("duration", strconv.Itoa(t.dur))
	}
	if scrobble {
		p.Set("timestamp", strconv.FormatInt(t.start, 10))
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := l.call(ctx, method, p, true)
	return err
}

func (l *lastfm) run(ctx context.Context, st *state) {
	if l.cfg.APIKey == "" || l.cfg.APISecret == "" || l.cfg.SessionKey == "" {
		st.with(func(s *snap) { s.lfm = "Last.fm: not set up (run gr.exe -auth)" })
		return
	}
	st.with(func(s *snap) { s.lfm = "Last.fm: ready" })

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var (
		key           int64
		cur           track
		base          float64 // seconds of the song we did not hear
		first         = true
		nowSent, done bool
		tries         int
		retryAt       time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sn := st.get()
		if !sn.ok {
			continue
		}
		r := &sn.r
		if k := r.SongTimes.SongStart; k != key {
			key = k
			cur = track{r.SongInfo.Artist, r.SongInfo.Title, r.SongInfo.Album, k, r.SongTimes.Duration}
			base = 0
			if first {
				base = sn.playedNow() // we joined mid-song
			}
			first = false
			nowSent, done, tries = false, false, 0
			retryAt = time.Time{}
		}
		if cur.title == "" || cur.artist == "" {
			continue
		}
		if !nowSent {
			nowSent = true
			if err := l.send(ctx, "track.updateNowPlaying", cur, false); err != nil {
				msg := "Last.fm: now playing failed: " + err.Error()
				st.with(func(s *snap) { s.lfm = msg })
			} else {
				st.with(func(s *snap) { s.lfm = "Last.fm: now playing sent" })
			}
		}
		if done || cur.dur < 30 || time.Now().Before(retryAt) {
			continue
		}
		thr := float64(min(cur.dur/2, maxScrobTh))
		if sn.playedNow()-base < thr {
			continue
		}
		if err := l.send(ctx, "track.scrobble", cur, true); err != nil {
			tries++
			retryAt = time.Now().Add(30 * time.Second)
			if tries >= 5 {
				done = true
			}
			msg := "Last.fm: scrobble failed: " + err.Error()
			st.with(func(s *snap) { s.lfm = msg })
		} else {
			done = true
			st.with(func(s *snap) { s.lfm = "Last.fm: scrobbled" })
		}
	}
}

func authorize(path string, cfg config, c *http.Client) error {
	if cfg.APIKey == "" || cfg.APISecret == "" {
		return errors.New("put api_key and api_secret in " + path + " first")
	}
	l := &lastfm{c: c, cfg: cfg}
	ctx := context.Background()
	r, err := l.call(ctx, "auth.getToken", url.Values{}, false)
	if err != nil {
		return err
	}
	link := "https://www.last.fm/api/auth/?api_key=" + url.QueryEscape(cfg.APIKey) + "&token=" + url.QueryEscape(r.Token)
	fmt.Println("Allow access in your browser, then press Enter here.")
	fmt.Println(link)
	openURL(link)
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	r2, err := l.call(ctx, "auth.getSession", url.Values{"token": {r.Token}}, false)
	if err != nil {
		return err
	}
	cfg.SessionKey = r2.Session.Key
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	fmt.Println("Saved session key to", path)
	return nil
}

// ---------------------------------------------------------------- TUI

var (
	k32        = windows.NewLazySystemDLL("kernel32.dll")
	procGetCP  = k32.NewProc("GetConsoleOutputCP")
	procSetCP  = k32.NewProc("SetConsoleOutputCP")
	oldOutMode uint32
	oldInMode  uint32
)

func setupConsole() func() {
	out := windows.Handle(os.Stdout.Fd())
	in := windows.Handle(os.Stdin.Fd())
	okOut := windows.GetConsoleMode(out, &oldOutMode) == nil
	okIn := windows.GetConsoleMode(in, &oldInMode) == nil
	oldCP, _, _ := procGetCP.Call()
	if okOut {
		_ = windows.SetConsoleMode(out, oldOutMode|windows.ENABLE_PROCESSED_OUTPUT|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	if okIn {
		// Keep ENABLE_PROCESSED_INPUT so Ctrl+C still works.
		_ = windows.SetConsoleMode(in, oldInMode&^(windows.ENABLE_LINE_INPUT|windows.ENABLE_ECHO_INPUT))
	}
	_, _, _ = procSetCP.Call(65001)
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J")
	return func() {
		os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
		if okOut {
			_ = windows.SetConsoleMode(out, oldOutMode)
		}
		if okIn {
			_ = windows.SetConsoleMode(in, oldInMode)
		}
		if oldCP != 0 {
			_, _, _ = procSetCP.Call(oldCP)
		}
	}
}

func consoleWidth() int {
	var info windows.ConsoleScreenBufferInfo
	if windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info) != nil {
		return 80
	}
	return int(info.Window.Right-info.Window.Left) + 1
}

func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F,
		r >= 0x2E80 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0xFE30 && r <= 0xFE6F,
		r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

// fit shortens s to at most max terminal columns.
func fit(s string, max int) string {
	if max < 2 {
		return ""
	}
	w, cut := 0, -1
	for i, r := range s {
		rw := runeWidth(r)
		if cut < 0 && w+rw > max-1 {
			cut = i
		}
		w += rw
	}
	if w <= max {
		return s
	}
	return s[:cut] + "…"
}

func fmtDur(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	t := int(sec)
	h, m, s := t/3600, t%3600/60, t%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func fmtRating(s string, n int) string {
	if n == 0 || s == "" {
		return "n/a"
	}
	v, _, _ := strings.Cut(s, "/")
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return s
	}
	return fmt.Sprintf("%.2f/5.0 (%d)", math.Round(f*100+1e-9)/100, n)
}

func bar(frac float64, w int) string {
	if frac != frac || frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	n := int(frac * float64(w))
	return "[" + strings.Repeat("#", n) + strings.Repeat("-", w-n) + "] " + fmt.Sprintf("%3d%%", int(frac*100))
}

func render(buf *bytes.Buffer, sn snap, width int) {
	if width < 24 {
		width = 24
	}
	buf.WriteString("\x1b[H")
	line := func(s string) {
		buf.WriteString(fit(s, width-1))
		buf.WriteString("\x1b[K\r\n")
	}
	r := &sn.r
	line("Gensokyo Radio")
	line("")
	if !sn.ok {
		line("Waiting for metadata...")
	} else {
		album := r.SongInfo.Album
		if r.SongInfo.Year != "" {
			album += " (" + r.SongInfo.Year + ")"
		}
		line("Title      " + r.SongInfo.Title)
		line("Artist     " + r.SongInfo.Artist)
		line("Album      " + album)
		line("Circle     " + r.SongInfo.Circle)
		line("")
		line("Rating     " + fmtRating(r.SongData.Rating, r.SongData.TimesRated))
		line(fmt.Sprintf("Listeners  %d (%d on this stream)", r.ServerInfo.Listeners, r.ServerInfo.Streams[streamKey].Listeners))
		line("")
		dur := float64(r.SongTimes.Duration)
		played := sn.playedNow()
		line(fmt.Sprintf("Song       %s / %s   remaining %s", fmtDur(played), fmtDur(dur), fmtDur(dur-played)))
		bw := width - 12
		if bw > 50 {
			bw = 50
		}
		if bw < 10 {
			bw = 10
		}
		frac := 0.0
		if dur > 0 {
			frac = played / dur
		}
		line("           " + bar(frac, bw))
	}
	line("Total      " + fmtDur(sn.listened().Seconds()) + " listened")
	line("")
	status := sn.mpv
	if status == "" {
		status = "mpv: starting"
	}
	line(status)
	line(sn.lfm)
	if sn.apiErr != "" {
		line("API: " + sn.apiErr)
	} else {
		line("")
	}
	line("")
	line("G  search Google    Q  quit")
	buf.WriteString("\x1b[J")
}

func openURL(u string) {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

func openGoogle(sn snap) {
	if !sn.ok {
		return
	}
	q := sn.r.SongInfo.Artist + " - " + sn.r.SongInfo.Title
	if a := sn.r.SongInfo.Album; a != "" {
		q += " (" + a + ")"
	}
	openURL("https://www.google.com/search?q=" + url.QueryEscape(q))
}

func readKeys(st *state, cancel context.CancelFunc) {
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil || n == 0 {
			return
		}
		switch b[0] {
		case 'q', 'Q':
			cancel()
			return
		case 'g', 'G':
			openGoogle(st.get())
		}
	}
}

// ---------------------------------------------------------------- main

func main() {
	debug.SetGCPercent(20)
	debug.SetMemoryLimit(8 << 20)

	authMode := flag.Bool("auth", false, "authorize this program with your Last.fm account")
	flag.Parse()

	path := configPath()
	cfg := loadConfig(path)
	client := newClient()

	if *authMode {
		if err := authorize(path, cfg, client); err != nil {
			fmt.Fprintln(os.Stderr, "auth failed:", err)
			os.Exit(1)
		}
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	job := newKillJob()
	restore := setupConsole()

	st := &state{}
	lf := &lastfm{c: client, cfg: cfg}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runMPV(ctx, st, job)
	}()
	go poll(ctx, st, client)
	go lf.run(ctx, st)
	go readKeys(st, cancel)

	var buf bytes.Buffer
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		render(&buf, st.get(), consoleWidth())
		os.Stdout.Write(buf.Bytes())
		buf.Reset()
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
		}
	}
	restore()
	wg.Wait() // mpv is killed by the context; wait until it has exited
}
