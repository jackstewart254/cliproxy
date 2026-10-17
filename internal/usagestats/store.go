// Package usagestats keeps a small persisted tally of upstream usage per
// credential and per hour, daily tallies per provider and per credential that
// are never pruned, request timing per credential (hourly for 35 days, then
// one rolled up bucket per local day), plus the credentials each client
// session used. It backs the /dashboard page and the usage summary management
// endpoint.
package usagestats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	hourlyRetention  = 14 * 24 * time.Hour
	sessionRetention = 48 * time.Hour
	maxSessions      = 500
	flushInterval    = time.Minute
	// statsFileVersion 3 added the per-credential daily tally, the daily
	// timing rollups and token sums in the timing buckets.
	statsFileVersion = 3
	dayLayout        = "2006-01-02"

	// HistoryFileName is the read-only backfill that sits next to the stats
	// file. It holds daily totals rebuilt from local Claude Code and Codex logs
	// for the time before the proxy kept its own tally.
	HistoryFileName = "usage-history.json"
)

// Counters is one bucket of usage for a credential. APICost is what the
// tokens would have cost at the public API list prices, in USD.
type Counters struct {
	Requests   int64   `json:"requests"`
	Failed     int64   `json:"failed"`
	Input      int64   `json:"input_tokens"`
	Output     int64   `json:"output_tokens"`
	CacheRead  int64   `json:"cache_read_tokens"`
	CacheWrite int64   `json:"cache_write_tokens"`
	APICost    float64 `json:"api_cost"`
}

func (c *Counters) add(other Counters) {
	c.Requests += other.Requests
	c.Failed += other.Failed
	c.Input += other.Input
	c.Output += other.Output
	c.CacheRead += other.CacheRead
	c.CacheWrite += other.CacheWrite
	c.APICost = roundCost(c.APICost + other.APICost)
}

// tokens is the sum of every token count.
func (c Counters) tokens() int64 { return c.Input + c.Output + c.CacheRead + c.CacheWrite }

// Session records which credentials served one client session.
type Session struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Provider string `json:"provider"`
	// Title and Machine come from POST /dashboard/sessions; Machine falls back
	// to the device named in the client key (see deviceFromClientKey), then to
	// this host's name for a loopback client. Both are filled in by Summary.
	Title         string                `json:"title"`
	Machine       string                `json:"machine"`
	Model         string                `json:"model"`
	ServingAuthID string                `json:"serving_auth_id"`
	AuthIDs       []string              `json:"auth_ids"`
	ByAuth        map[string]*AuthCount `json:"by_auth"`
	FirstSeen     time.Time             `json:"first_seen"`
	LastSeen      time.Time             `json:"last_seen"`
	TTFTP50       int64                 `json:"ttft_ms_p50"`
	// ClientIP, Device and TTFTHist are kept in the stats file and left out of
	// Summary. ServedAt is when the request that set ServingAuthID started.
	ClientIP string    `json:"client_ip,omitempty"`
	Device   string    `json:"device,omitempty"`
	TTFTHist histogram `json:"ttft_hist,omitempty"`
	ServedAt time.Time `json:"served_at,omitzero"`
	Counters
}

// AuthCount is how many of a session's requests went to one credential.
type AuthCount struct {
	Requests int64 `json:"requests"`
	Failed   int64 `json:"failed"`
}

type fileState struct {
	Version  int                             `json:"version"`
	Hourly   map[string]map[int64]*Counters  `json:"hourly"`
	Daily    map[string]map[string]*Counters `json:"daily,omitempty"`
	Sessions map[string]*Session             `json:"sessions"`
	// Perf is hourly request timing per credential, and PerfDaily the older
	// hours rolled up per credential and local day.
	Perf          map[string]map[int64]*perfBucket  `json:"perf,omitempty"`
	PerfDaily     map[string]map[string]*perfBucket `json:"perf_daily,omitempty"`
	AuthProviders map[string]string                 `json:"auth_providers,omitempty"`
	SessionMeta   map[string]*sessionMeta           `json:"session_meta,omitempty"`
	// AccountDaily is usage per credential and local day.
	AccountDaily map[string]map[string]*Counters `json:"account_daily,omitempty"`
	// CostSince is when per-request pricing started. A file without it was
	// written before API cost and gets a one-time estimate on load.
	CostSince time.Time `json:"cost_since,omitzero"`
}

// historyFile is the backfill written by the log collector. Days and machines
// map to provider totals; requests there count model replies. Models splits
// some days' provider totals by model, keyed by day, provider and model id,
// and MachineModels does the same for machines.
type historyFile struct {
	Cutoff        time.Time                                 `json:"cutoff"`
	Days          map[string]map[string]Counters            `json:"days"`
	Machines      map[string]map[string]Counters            `json:"machines,omitempty"`
	Models        map[string]map[string]map[string]Counters `json:"models,omitempty"`
	MachineModels map[string]map[string]map[string]Counters `json:"machine_models,omitempty"`
}

// Store aggregates usage records in memory and flushes them to disk.
type Store struct {
	mu       sync.Mutex
	path     string
	hourly   map[string]map[int64]*Counters
	daily    map[string]map[string]*Counters
	sessions map[string]*Session
	// accountDaily is usage per credential and local day, never pruned.
	accountDaily map[string]map[string]*Counters
	// perf is hourly request timing per credential and perfDaily the hours
	// older than perfRetention rolled up per local day; a request is in one
	// or the other. authProviders remembers each credential's provider for
	// the provider scopes.
	perf          map[string]map[int64]*perfBucket
	perfDaily     map[string]map[string]*perfBucket
	authProviders map[string]string
	// traces follows recent inbound requests that had a failed attempt.
	traces   map[string]*traceState
	meta     map[string]*sessionMeta
	backfill *historyFile
	// fallback names each provider's model for pricing unknown models and
	// usage from before costSince, when per-request pricing started.
	fallback  map[string]string
	costSince time.Time
	// changes counts mutations and saved is the count the stats file holds;
	// the tally needs saving while they differ.
	changes uint64
	saved   uint64
	// persistOff stops every save after the stats file failed to load, so a
	// near-empty tally never replaces it.
	persistOff bool
	// flushMu serializes whole flushes, from snapshot to rename, so an older
	// snapshot never replaces a newer one. It is taken before mu.
	flushMu sync.Mutex
	// saveFile writes a snapshot to the stats file path.
	saveFile func(path string, data []byte) error
	nowFunc  func() time.Time
	// machineName maps a client address to a short machine name.
	machineName func(ip string) string
	stopOnce    sync.Once
	stop        chan struct{}
}

var defaultStore = newStore()

func init() {
	coreusage.RegisterPlugin(defaultStore)
}

func newStore() *Store {
	return &Store{
		hourly:        make(map[string]map[int64]*Counters),
		daily:         make(map[string]map[string]*Counters),
		sessions:      make(map[string]*Session),
		accountDaily:  make(map[string]map[string]*Counters),
		perf:          make(map[string]map[int64]*perfBucket),
		perfDaily:     make(map[string]map[string]*perfBucket),
		authProviders: make(map[string]string),
		traces:        make(map[string]*traceState),
		meta:          make(map[string]*sessionMeta),
		fallback:      defaultFallback(),
		saveFile:      writeFileAtomic,
		nowFunc:       time.Now,
		machineName:   localMachineName,
	}
}

// Default returns the process-wide store.
func Default() *Store { return defaultStore }

// localMachineName is this host's name for a loopback client address, else "".
func localMachineName(ip string) string {
	if parsed := net.ParseIP(strings.TrimSpace(ip)); parsed != nil && parsed.IsLoopback() {
		return HostName()
	}
	return ""
}

// deviceFromClientKey is the device a client key was issued to, or "". Keys
// that scripts/pc/hub-add-device.ps1 issues read "dev.<device>.<secret>", so a
// hub can name the Mac behind each request although Tailscale Serve makes every
// one arrive from loopback. Only the name is kept, never the key.
func deviceFromClientKey(key string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(key), "dev.")
	if !ok {
		return ""
	}
	name, secret, ok := strings.Cut(rest, ".")
	if !ok || secret == "" || name == "" || len(name) > maxMachineLength {
		return ""
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return ""
		}
	}
	return strings.Clone(name)
}

// HostName is this host's short name, or "" when the OS does not say.
func HostName() string {
	host, errHost := os.Hostname()
	if errHost != nil {
		return ""
	}
	name, _, _ := strings.Cut(host, ".")
	return name
}

// Configure sets the persistence file, loads any saved state and starts the
// periodic flush. Calling it again with the same path is a no-op.
func Configure(path string) {
	defaultStore.configure(path)
}

func (s *Store) configure(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	s.mu.Lock()
	if s.path == path {
		s.mu.Unlock()
		return
	}
	s.path = path
	// The history comes first: it sets the fallback models that price an
	// older stats file.
	s.loadHistoryLocked(filepath.Join(filepath.Dir(path), HistoryFileName))
	estimated := s.loadLocked()
	s.mu.Unlock()
	if estimated {
		// Saves the one-time estimate together with cost_since.
		if errFlush := s.Flush(); errFlush != nil {
			log.Warnf("usagestats: save after pricing earlier usage failed: %v", errFlush)
		}
	}

	s.stopOnce.Do(func() {
		s.stop = make(chan struct{})
		go s.flushLoop()
	})
}

func (s *Store) flushLoop() {
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if errFlush := s.Flush(); errFlush != nil {
				log.Warnf("usagestats: flush failed: %v", errFlush)
			}
		case <-s.stop:
			return
		}
	}
}

// HandleUsage implements coreusage.Plugin.
func (s *Store) HandleUsage(ctx context.Context, record coreusage.Record) {
	if s == nil || !redisqueue.UsageStatisticsEnabled() {
		return
	}
	meta := internallogging.GetClientRequestMetadata(ctx)
	clientIP := strings.TrimSpace(meta.ClientIP)
	if clientIP == "" {
		clientIP = strings.TrimSpace(meta.ResolvedClientIP)
	}
	s.recordFrom(record, clientIP)
}

func (s *Store) record(record coreusage.Record) {
	s.recordFrom(record, "")
}

// recordFrom adds one upstream attempt. clientIP is the address of the client
// that sent the inbound request, or "".
func (s *Store) recordFrom(record coreusage.Record, clientIP string) {
	authID := strings.TrimSpace(record.AuthID)
	if authID == "" {
		return
	}
	at := record.RequestedAt
	if at.IsZero() {
		at = s.nowFunc()
	}
	delta := countersFromRecord(record)
	provider := providerOf(record.Provider, authID)

	location := s.nowFunc().Location()
	hourStart := bucketStart(at, location, 1)

	s.mu.Lock()
	defer s.mu.Unlock()
	delta.APICost = costFor(record.Model, provider, delta, s.fallback, true)
	buckets := s.hourly[authID]
	if buckets == nil {
		buckets = make(map[int64]*Counters)
		s.hourly[authID] = buckets
	}
	hour := hourStart.Unix()
	bucket := buckets[hour]
	if bucket == nil {
		bucket = &Counters{}
		buckets[hour] = bucket
	}
	bucket.add(delta)

	day := at.In(location).Format(dayLayout)
	s.dailyBucketLocked(day, provider).add(delta)
	s.accountDayLocked(authID, day).add(delta)
	s.recordPerfLocked(authID, provider, hourStart, recordTiming{
		traceID: strings.TrimSpace(record.TraceID),
		at:      at,
		stream:  record.UpstreamStream,
		failed:  record.Failed,
		ttft:    record.TTFT,
		latency: record.Latency,
		tokens:  delta,
	})

	if sessionID := strings.TrimSpace(record.SessionID); sessionID != "" {
		session := s.sessions[sessionID]
		if session == nil {
			session = &Session{ID: sessionID, Provider: strings.TrimSpace(record.Provider), FirstSeen: at}
			s.sessions[sessionID] = session
		}
		// Only accounts that answered count as serving the session. A failed try
		// that fell back to another account did not move its prompt cache.
		if !record.Failed && !containsString(session.AuthIDs, authID) {
			session.AuthIDs = append(session.AuthIDs, authID)
		}
		if session.ByAuth == nil {
			session.ByAuth = make(map[string]*AuthCount)
		}
		count := session.ByAuth[authID]
		if count == nil {
			count = &AuthCount{}
			session.ByAuth[authID] = count
		}
		count.Requests++
		if record.Failed {
			count.Failed++
		} else {
			// Attempts finish out of order: the serving account is the one that
			// answered the latest request, not the last one to finish.
			if !at.Before(session.ServedAt) {
				session.ServingAuthID = authID
				session.ServedAt = at
			}
			if record.TTFT > 0 {
				session.TTFTHist.observe(float64(record.TTFT)/float64(time.Millisecond), ttftBase)
			}
		}
		// Model and client address describe the request at LastSeen.
		if !at.Before(session.LastSeen) {
			if model := strings.TrimSpace(record.Model); model != "" {
				session.Model = model
			}
			if clientIP != "" {
				session.ClientIP = clientIP
			}
			if device := deviceFromClientKey(record.APIKey); device != "" {
				session.Device = device
			}
			session.LastSeen = at
		}
		session.Counters.add(delta)
	}
	s.markDirtyLocked()
}

// markDirtyLocked records that the tally changed since the last save.
func (s *Store) markDirtyLocked() { s.changes++ }

func (s *Store) dailyBucketLocked(day, provider string) *Counters {
	providers := s.daily[day]
	if providers == nil {
		providers = make(map[string]*Counters)
		s.daily[day] = providers
	}
	bucket := providers[provider]
	if bucket == nil {
		bucket = &Counters{}
		providers[provider] = bucket
	}
	return bucket
}

// accountDayLocked returns a credential's usage for one local day.
func (s *Store) accountDayLocked(authID, day string) *Counters {
	days := s.accountDaily[authID]
	if days == nil {
		days = make(map[string]*Counters)
		s.accountDaily[authID] = days
	}
	bucket := days[day]
	if bucket == nil {
		bucket = &Counters{}
		days[day] = bucket
	}
	return bucket
}

// providerOf names the provider for the daily tally. Auth file names start
// with the provider ("claude-", "codex-"), which covers records that carry
// no provider and the hourly buckets seeded from an older stats file.
func providerOf(provider, authID string) string {
	if provider = strings.ToLower(strings.TrimSpace(provider)); provider != "" {
		return provider
	}
	if prefix, _, ok := strings.Cut(authID, "-"); ok && prefix != "" {
		return strings.ToLower(prefix)
	}
	return "unknown"
}

func countersFromRecord(record coreusage.Record) Counters {
	counters := Counters{Requests: 1}
	if record.Failed {
		counters.Failed = 1
	}
	detail := record.Detail
	if breakdown := detail.TokenBreakdown; breakdown.Valid() {
		counters.Input = breakdown.Input.UncachedTokens
		counters.CacheRead = breakdown.Input.CacheReadTokens
		counters.CacheWrite = breakdown.Input.CacheWriteTokens
		counters.Output = breakdown.Output.TotalTokens
		return counters
	}
	counters.CacheRead = detail.CacheReadTokens
	if counters.CacheRead == 0 {
		counters.CacheRead = detail.CachedTokens
	}
	counters.CacheWrite = detail.CacheCreationTokens
	counters.Input = detail.InputTokens
	counters.Output = detail.OutputTokens
	return counters
}

// Summary is the dashboard view of the tally.
type Summary struct {
	GeneratedAt time.Time                 `json:"generated_at"`
	Timezone    string                    `json:"timezone"`
	Accounts    map[string]AccountSummary `json:"accounts"`
	Sessions    []Session                 `json:"sessions"`
	Totals      map[string]Counters       `json:"totals"`
	History     History                   `json:"history"`
	Range       string                    `json:"range"`
	Performance Performance               `json:"performance"`
	UsageRange  UsageRange                `json:"usage_range"`
	Pricing     Pricing                   `json:"pricing"`
}

// UsageRange is usage per credential over the selected range, in the same
// buckets as the performance series. Accounts with no usage in the range are
// left out, and Ranges are the range keys the dashboard offers for usage,
// measured on the earliest history day.
type UsageRange struct {
	Range         string                `json:"range"`
	BucketSeconds int64                 `json:"bucket_seconds"`
	Starts        []time.Time           `json:"starts"`
	Ends          []time.Time           `json:"ends,omitempty"`
	Accounts      map[string][]Counters `json:"accounts"`
	Ranges        []string              `json:"ranges"`
	// ViewStart and ViewEnd are the window the bucket length was chosen for,
	// set only when the buckets were padded around it (UsageBetweenPadded).
	ViewStart *time.Time `json:"view_start,omitempty"`
	ViewEnd   *time.Time `json:"view_end,omitempty"`
}

// History is the long view: one entry per day with usage, oldest first, from
// the log backfill before BackfillCutoff and the proxy's own tally after it.
type History struct {
	BackfillCutoff   *time.Time                     `json:"backfill_cutoff,omitempty"`
	BackfillMachines map[string]map[string]Counters `json:"backfill_machines,omitempty"`
	Days             []HistoryDay                   `json:"days"`
	Today            Counters                       `json:"today"`
	ThisWeek         Counters                       `json:"this_week"`
	ThisMonth        Counters                       `json:"this_month"`
	Lifetime         Counters                       `json:"lifetime"`
}

// HistoryDay is one local calendar day of usage split by provider.
type HistoryDay struct {
	Date      string              `json:"date"`
	Providers map[string]Counters `json:"providers"`
}

// AccountSummary holds one credential's totals, its last 48 hourly buckets
// and its last 14 local days. Longer per-credential ranges are in UsageRange.
type AccountSummary struct {
	Today   Counters     `json:"today"`
	Last24h Counters     `json:"last_24h"`
	Last7d  Counters     `json:"last_7d"`
	Hourly  []HourBucket `json:"hourly"`
	Daily   []DayBucket  `json:"daily"`
}

// DayBucket is one local calendar day of usage.
type DayBucket struct {
	Date string `json:"date"`
	Counters
}

// accountDays is the number of local days in AccountSummary.Daily.
const accountDays = 14

// HourBucket is one hour of usage, keyed by the hour's start time.
type HourBucket struct {
	Start time.Time `json:"start"`
	Counters
}

// Summary builds the dashboard view with the default performance range.
// sessionLimit caps the session list.
func (s *Store) Summary(sessionLimit int) Summary {
	window, _ := LookupWindow(DefaultWindow)
	return s.SummaryFor(sessionLimit, window)
}

// SummaryFor builds the dashboard view with performance over window.
func (s *Store) SummaryFor(sessionLimit int, window Window) Summary {
	return s.SummaryForSelection(sessionLimit, window, nil, nil)
}

// SummaryForSelection is SummaryFor plus a performance scope named
// SelectionScope that merges the credentials in ids. Ids that the tally has
// not seen and known does not list are ignored, and only the first
// MaxSelectionIDs count. Without a known id there is no SelectionScope.
func (s *Store) SummaryForSelection(sessionLimit int, window Window, ids, known []string) Summary {
	now := s.nowFunc()
	startOfDay := bucketStart(now, now.Location(), 24)
	hourBounds := localBounds(now, 1, 48)
	dayStarts := dayBounds(now, accountDays, 1)
	dates := make([]string, accountDays)
	for i := range dates {
		dates[i] = dayStarts[i].Format(dayLayout)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)

	summary := Summary{
		GeneratedAt: now,
		Timezone:    zoneName(now.Location()),
		Accounts:    make(map[string]AccountSummary, len(s.hourly)),
		Totals:      map[string]Counters{},
	}
	var today, day, week Counters
	for authID, buckets := range s.hourly {
		var account AccountSummary
		hourly := make([]HourBucket, 48)
		for i := range hourly {
			hourly[i].Start = hourBounds[i]
		}
		// The daily view reads the per-credential daily tally, which holds the
		// same requests as the hourly buckets and outlives them.
		daily := make([]DayBucket, accountDays)
		days := s.accountDaily[authID]
		for i := range daily {
			daily[i].Date = dates[i]
			if counters := days[dates[i]]; counters != nil {
				daily[i].Counters = *counters
			}
		}
		for hourUnix, bucket := range buckets {
			start := time.Unix(hourUnix, 0)
			if !start.Before(startOfDay) {
				account.Today.add(*bucket)
			}
			if now.Sub(start) < 24*time.Hour {
				account.Last24h.add(*bucket)
			}
			if now.Sub(start) < 7*24*time.Hour {
				account.Last7d.add(*bucket)
			}
			// Stored hours from older files may sit on another grid. One that
			// starts before the first hour is out of range, not part of it.
			if index, ok := boundsIndex(hourBounds, start); ok {
				hourly[index].Counters.add(*bucket)
			}
		}
		account.Hourly = hourly
		account.Daily = daily
		summary.Accounts[authID] = account
		today.add(account.Today)
		day.add(account.Last24h)
		week.add(account.Last7d)
	}
	summary.Totals["today"] = today
	summary.Totals["last_24h"] = day
	summary.Totals["last_7d"] = week
	summary.History = s.historyLocked(now)
	summary.Range = window.Key
	summary.Performance = s.performanceLocked(now, window, s.knownSelectionLocked(ids, known))
	summary.UsageRange = s.usageRangeLocked(now, window, summary.History.Days)
	summary.Pricing = s.pricingLocked()

	sessions := make([]Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		sessions = append(sessions, s.sessionViewLocked(session))
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastSeen.After(sessions[j].LastSeen) })
	if sessionLimit > 0 && len(sessions) > sessionLimit {
		sessions = sessions[:sessionLimit]
	}
	summary.Sessions = sessions
	return summary
}

// usageRangeLocked builds per-credential usage for window ending at now. Day
// sized buckets read the per-credential daily tally, shorter ones the hourly
// buckets; both hold the same requests, so only one is read. days is the
// history, which sets where "all" starts and whether 180d is offered.
func (s *Store) usageRangeLocked(now time.Time, window Window, days []HistoryDay) UsageRange {
	loc := now.Location()
	dates := make([]string, len(days))
	for i, day := range days {
		dates[i] = day.Date
	}
	first, _ := earliestDateStart(dates, loc)
	bounds, bucketLength := window.span(now, first)
	count := len(bounds) - 1
	usage := UsageRange{
		Range:         window.Key,
		BucketSeconds: int64(bucketLength / time.Second),
		Starts:        bounds[:count],
		Accounts:      make(map[string][]Counters),
		Ranges:        availableRanges(now, first),
	}
	add := func(authID string, at time.Time, counters *Counters) {
		index, ok := boundsIndex(bounds, at)
		if !ok {
			return
		}
		series := usage.Accounts[authID]
		if series == nil {
			series = make([]Counters, count)
			usage.Accounts[authID] = series
		}
		series[index].add(*counters)
	}
	if window.daily() {
		for authID, dates := range s.accountDaily {
			for date, counters := range dates {
				if noon, ok := dateNoon(date, loc); ok {
					add(authID, noon, counters)
				}
			}
		}
		return usage
	}
	for authID, buckets := range s.hourly {
		for hourUnix, counters := range buckets {
			add(authID, time.Unix(hourUnix, 0), counters)
		}
	}
	return usage
}

// sessionViewLocked copies a session for Summary and fills in the derived
// fields.
func (s *Store) sessionViewLocked(session *Session) Session {
	view := *session
	view.AuthIDs = append([]string{}, session.AuthIDs...)
	view.ByAuth = make(map[string]*AuthCount, len(session.ByAuth))
	for authID, count := range session.ByAuth {
		if count == nil {
			continue
		}
		copyCount := *count
		view.ByAuth[authID] = &copyCount
	}
	view.ParentID = parentSessionID(session.ID)
	view.TTFTP50 = roundMillis(session.TTFTHist.percentile(0.5, ttftBase))
	view.TTFTHist = nil
	view.ClientIP = ""
	view.Device = ""
	base, agent := splitSessionID(session.ID)
	if meta := s.meta[base]; meta != nil {
		view.Machine = meta.Machine
		// Agent threads inherit the machine; the title names the main session.
		if !agent {
			view.Title = meta.Title
		}
	}
	if view.Machine == "" {
		view.Machine = session.Device
	}
	if view.Machine == "" && session.ClientIP != "" && s.machineName != nil {
		view.Machine = s.machineName(session.ClientIP)
	}
	return view
}

func (s *Store) historyLocked(now time.Time) History {
	merged := make(map[string]map[string]Counters)
	addDay := func(day, provider string, counters Counters) {
		providers := merged[day]
		if providers == nil {
			providers = make(map[string]Counters)
			merged[day] = providers
		}
		total := providers[provider]
		total.add(counters)
		providers[provider] = total
	}
	var history History
	if s.backfill != nil {
		cutoff := s.backfill.Cutoff
		if !cutoff.IsZero() {
			history.BackfillCutoff = &cutoff
		}
		history.BackfillMachines = s.backfill.Machines
		for day, providers := range s.backfill.Days {
			for provider, counters := range providers {
				addDay(day, provider, counters)
			}
		}
	}
	for day, providers := range s.daily {
		for provider, counters := range providers {
			addDay(day, provider, *counters)
		}
	}

	today := now.Format(dayLayout)
	weekday := (int(now.Weekday()) + 6) % 7 // Monday is 0
	weekStart := time.Date(now.Year(), now.Month(), now.Day()-weekday, 0, 0, 0, 0, now.Location()).Format(dayLayout)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format(dayLayout)

	history.Days = make([]HistoryDay, 0, len(merged))
	for day, providers := range merged {
		history.Days = append(history.Days, HistoryDay{Date: day, Providers: providers})
		for _, counters := range providers {
			history.Lifetime.add(counters)
			if day > today {
				continue
			}
			if day == today {
				history.Today.add(counters)
			}
			if day >= weekStart {
				history.ThisWeek.add(counters)
			}
			if day >= monthStart {
				history.ThisMonth.add(counters)
			}
		}
	}
	sort.Slice(history.Days, func(i, j int) bool { return history.Days[i].Date < history.Days[j].Date })
	return history
}

// hourlyCutoff is the oldest hour start the hourly usage buckets keep: the
// earliest bucket start of every view read from them, and never later than
// hourlyRetention before now. A window that crosses the change back from
// summer time is an hour longer than its usual length.
func hourlyCutoff(now time.Time) time.Time {
	cutoff := now.Add(-hourlyRetention)
	if first := localBounds(now, 1, 48)[0]; first.Before(cutoff) {
		cutoff = first
	}
	for _, window := range windows {
		if window.daily() {
			continue
		}
		if bounds, _ := window.span(now, time.Time{}); bounds[0].Before(cutoff) {
			cutoff = bounds[0]
		}
	}
	return cutoff
}

func (s *Store) pruneLocked(now time.Time) {
	hourCutoff := hourlyCutoff(now).Unix()
	for authID, buckets := range s.hourly {
		for hourUnix := range buckets {
			if hourUnix < hourCutoff {
				delete(buckets, hourUnix)
				s.markDirtyLocked()
			}
		}
		if len(buckets) == 0 {
			delete(s.hourly, authID)
		}
	}
	if s.rollUpPerfLocked(now.Add(-perfRetention)) {
		s.markDirtyLocked()
	}
	s.pruneSessionMetaLocked(now)
	sessionCutoff := now.Add(-sessionRetention)
	for id, session := range s.sessions {
		if session.LastSeen.Before(sessionCutoff) {
			delete(s.sessions, id)
			s.markDirtyLocked()
		}
	}
	if len(s.sessions) > maxSessions {
		sessions := make([]*Session, 0, len(s.sessions))
		for _, session := range s.sessions {
			sessions = append(sessions, session)
		}
		sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastSeen.After(sessions[j].LastSeen) })
		for _, session := range sessions[maxSessions:] {
			delete(s.sessions, session.ID)
		}
		s.markDirtyLocked()
	}
}

// Flush writes the tally to disk when it changed since the last save. Flushes
// run one at a time, and the tally counts as saved only once the new file is
// in place, so a failed write is retried by the next flush.
func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if s.path == "" || s.persistOff {
		s.mu.Unlock()
		return nil
	}
	s.pruneLocked(s.nowFunc())
	if s.changes == s.saved {
		s.mu.Unlock()
		return nil
	}
	generation := s.changes
	data, errMarshal := json.Marshal(fileState{
		Version:       statsFileVersion,
		Hourly:        s.hourly,
		Daily:         s.daily,
		Sessions:      s.sessions,
		Perf:          s.perf,
		PerfDaily:     s.perfDaily,
		AuthProviders: s.authProviders,
		SessionMeta:   s.meta,
		AccountDaily:  s.accountDaily,
		CostSince:     s.costSince,
	})
	path := s.path
	save := s.saveFile
	s.mu.Unlock()
	if errMarshal != nil {
		return errMarshal
	}
	// Records that arrive while the file is written raise changes past
	// generation, so the tally stays unsaved until the next flush.
	if errSave := save(path, data); errSave != nil {
		return errSave
	}
	s.mu.Lock()
	s.saved = generation
	s.mu.Unlock()
	return nil
}

// writeFileAtomic writes data to a temporary file next to path, syncs it and
// renames it over path, so path holds either the old or the new data.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	file, errOpen := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if errOpen != nil {
		return errOpen
	}
	_, errWrite := file.Write(data)
	if errWrite == nil {
		errWrite = file.Sync()
	}
	if errClose := file.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		errWrite = os.Rename(tmp, path)
	}
	if errWrite != nil {
		if errRemove := os.Remove(tmp); errRemove != nil && !os.IsNotExist(errRemove) {
			log.Warnf("usagestats: remove %s: %v", filepath.Base(tmp), errRemove)
		}
	}
	return errWrite
}

// failLoadLocked handles a stats file that exists but could not be read or
// parsed. It copies the file aside first and then turns saving off for this
// process, so the original is never replaced by a near-empty tally.
func (s *Store) failLoadLocked(errLoad error) {
	name := filepath.Base(s.path)
	backup := s.path + ".corrupt-" + s.nowFunc().UTC().Format("20060102T150405.000000000Z")
	errCopy := copyNewFile(s.path, backup)
	s.persistOff = true
	if errCopy != nil {
		log.Errorf("usagestats: cannot load %s: %v; copying it aside failed: %v; usage stats will not be saved until restart", name, errLoad, errCopy)
		return
	}
	log.Errorf("usagestats: cannot load %s: %v; copied it to %s; usage stats will not be saved until restart", name, errLoad, filepath.Base(backup))
}

// copyNewFile copies src to dst, which must not exist yet. A partial copy is
// removed.
func copyNewFile(src, dst string) error {
	in, errOpen := os.Open(src)
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		if errClose := in.Close(); errClose != nil {
			log.Warnf("usagestats: close %s: %v", filepath.Base(src), errClose)
		}
	}()
	out, errCreate := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errCreate != nil {
		return errCreate
	}
	_, errCopy := io.Copy(out, in)
	if errCopy == nil {
		errCopy = out.Sync()
	}
	if errClose := out.Close(); errCopy == nil {
		errCopy = errClose
	}
	if errCopy != nil {
		if errRemove := os.Remove(dst); errRemove != nil && !os.IsNotExist(errRemove) {
			log.Warnf("usagestats: remove %s: %v", filepath.Base(dst), errRemove)
		}
	}
	return errCopy
}

// loadLocked merges the stats file into the tally. It reports whether it
// estimated the API cost of a file written before per-request pricing, which
// happens once: the estimate is saved with cost_since.
func (s *Store) loadLocked() bool {
	// Without a usable file, every request from now on is priced exactly.
	s.costSince = s.nowFunc().Truncate(time.Second)
	data, errRead := os.ReadFile(s.path)
	if errRead != nil {
		if !os.IsNotExist(errRead) {
			s.failLoadLocked(fmt.Errorf("read: %w", errRead))
		}
		return false
	}
	var state fileState
	if errUnmarshal := json.Unmarshal(data, &state); errUnmarshal != nil {
		s.failLoadLocked(fmt.Errorf("parse: %w", errUnmarshal))
		return false
	}
	estimated := state.CostSince.IsZero()
	if estimated {
		estimateCost(&state, s.fallback)
		s.markDirtyLocked()
	} else {
		s.costSince = state.CostSince
	}
	for authID, buckets := range state.Hourly {
		target := s.hourly[authID]
		if target == nil {
			target = make(map[int64]*Counters, len(buckets))
			s.hourly[authID] = target
		}
		for hour, bucket := range buckets {
			if bucket == nil {
				continue
			}
			if existing := target[hour]; existing != nil {
				existing.add(*bucket)
			} else {
				copyBucket := *bucket
				target[hour] = &copyBucket
			}
		}
	}
	if state.Daily == nil {
		// Stats files from before the daily tally: rebuild it from the hourly
		// buckets so the days the proxy has already served are not lost.
		location := s.nowFunc().Location()
		for authID, buckets := range state.Hourly {
			for hour, bucket := range buckets {
				if bucket == nil {
					continue
				}
				day := time.Unix(hour, 0).In(location).Format(dayLayout)
				s.dailyBucketLocked(day, providerOf("", authID)).add(*bucket)
			}
		}
		if len(state.Hourly) > 0 {
			s.markDirtyLocked()
		}
	}
	for day, providers := range state.Daily {
		for provider, bucket := range providers {
			if bucket != nil {
				s.dailyBucketLocked(day, provider).add(*bucket)
			}
		}
	}
	if state.AccountDaily == nil {
		// Stats files from before the per-credential daily tally: rebuild it
		// from the hourly buckets, which hold the same requests.
		location := s.nowFunc().Location()
		for authID, buckets := range state.Hourly {
			for hour, bucket := range buckets {
				if bucket == nil {
					continue
				}
				day := time.Unix(hour, 0).In(location).Format(dayLayout)
				s.accountDayLocked(authID, day).add(*bucket)
				s.markDirtyLocked()
			}
		}
	}
	for authID, days := range state.AccountDaily {
		for day, bucket := range days {
			if bucket != nil {
				s.accountDayLocked(authID, day).add(*bucket)
			}
		}
	}
	for id, session := range state.Sessions {
		if session != nil && s.sessions[id] == nil {
			s.sessions[id] = loadedSession(id, session)
		}
	}
	for authID, buckets := range state.Perf {
		target := s.perf[authID]
		if target == nil {
			target = make(map[int64]*perfBucket, len(buckets))
			s.perf[authID] = target
		}
		for hour, bucket := range buckets {
			if bucket == nil {
				continue
			}
			bucket.validate()
			if state.Version < 3 && !bucket.hasTokens() {
				// Timing buckets from before version 3 have no token sums. The
				// hourly usage bucket with the same key holds the same requests.
				if counters := state.Hourly[authID][hour]; counters != nil {
					bucket.addTokens(*counters)
					s.markDirtyLocked()
				}
			}
			if existing := target[hour]; existing != nil {
				existing.add(bucket)
			} else {
				target[hour] = bucket
			}
		}
	}
	for authID, days := range state.PerfDaily {
		for day, bucket := range days {
			if bucket == nil {
				continue
			}
			bucket.validate()
			s.perfDayLocked(authID, day).add(bucket)
		}
	}
	for authID, provider := range state.AuthProviders {
		if s.authProviders[authID] == "" {
			s.authProviders[authID] = provider
		}
	}
	for id, meta := range state.SessionMeta {
		if meta != nil && s.meta[id] == nil {
			s.meta[id] = meta
		}
	}
	return estimated
}

// loadedSession repairs a session read from the stats file: the map key is
// its id, and nil per-account counts and invalid histogram entries are
// dropped so Summary never meets them.
func loadedSession(id string, session *Session) *Session {
	session.ID = id
	session.TTFTHist = session.TTFTHist.valid()
	for authID, count := range session.ByAuth {
		if count == nil {
			delete(session.ByAuth, authID)
		}
	}
	return session
}

// loadHistoryLocked reads the log backfill. A missing file is normal.
func (s *Store) loadHistoryLocked(path string) {
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		if !os.IsNotExist(errRead) {
			log.Warnf("usagestats: read %s: %v", filepath.Base(path), errRead)
		}
		return
	}
	var history historyFile
	if errUnmarshal := json.Unmarshal(data, &history); errUnmarshal != nil {
		log.Warnf("usagestats: parse %s: %v", filepath.Base(path), errUnmarshal)
		return
	}
	s.fallback = fallbackModels(&history)
	priceHistory(&history, s.fallback)
	s.backfill = &history
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
