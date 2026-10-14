package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// requestLogFile persists the bounded request log under data-dir.
const requestLogFile = "requests.json"

// maxRequestLogs bounds the in-memory and on-disk log ring.
const maxRequestLogs = 1000

// RequestLog is one completed (or failed) executor call, rendered by the
// console's log and statistics tabs.
type RequestLog struct {
	ID            string    `json:"id"`
	Time          time.Time `json:"time"`
	Model         string    `json:"model"`
	UpstreamModel string    `json:"upstream_model"`
	SourceFormat  string    `json:"source_format"`
	Stream        bool      `json:"stream"`
	Status        int       `json:"status"`
	DurationMS    int64     `json:"duration_ms"`
	FirstByteMS   int64     `json:"first_byte_ms"`
	InputTokens   int64     `json:"input_tokens"`
	OutputTokens  int64     `json:"output_tokens"`
	CachedTokens  int64     `json:"cached_tokens"`
	Error         string    `json:"error,omitempty"`
	AuthID        string    `json:"auth_id,omitempty"`
}

// telemetry keeps a bounded request log plus derived statistics. It is safe
// for concurrent use and persists to disk with a short debounce so a burst of
// streaming finishes coalesces into one write.
type telemetry struct {
	mu    sync.Mutex
	dir   string
	logs  []RequestLog
	timer *time.Timer
	dirty bool
}

func newTelemetry(dir string) *telemetry {
	t := &telemetry{dir: strings.TrimSpace(dir)}
	if t.dir != "" {
		if raw, err := os.ReadFile(filepath.Join(t.dir, requestLogFile)); err == nil {
			_ = json.Unmarshal(raw, &t.logs)
		}
	}
	return t
}

func logID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// record appends one entry, trims the ring, and schedules a persist.
func (t *telemetry) record(e RequestLog) {
	if t == nil {
		return
	}
	if e.ID == "" {
		e.ID = logID()
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	t.mu.Lock()
	t.logs = append(t.logs, e)
	if len(t.logs) > maxRequestLogs {
		t.logs = t.logs[len(t.logs)-maxRequestLogs:]
	}
	t.dirty = true
	if t.timer == nil {
		t.timer = time.AfterFunc(2*time.Second, t.flush)
	}
	t.mu.Unlock()
}

// flush writes the current log ring to disk when dirty and armed.
func (t *telemetry) flush() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.dirty || t.dir == "" {
		t.dirty = false
		t.timer = nil
		t.mu.Unlock()
		return
	}
	snapshot := make([]RequestLog, len(t.logs))
	copy(snapshot, t.logs)
	dir := t.dir
	t.dirty = false
	t.timer = nil
	t.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".requests-*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmpPath, filepath.Join(dir, requestLogFile))
}

// reset clears the log ring and persists the empty state.
func (t *telemetry) reset() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.logs = nil
	t.dirty = true
	if t.timer == nil {
		t.timer = time.AfterFunc(0, t.flush)
	}
	t.mu.Unlock()
}

// logSummary aggregates the filtered window for the header cards.
type logSummary struct {
	Requests     int     `json:"requests"`
	Success      int     `json:"success"`
	Failed       int     `json:"failed"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	CacheRate    float64 `json:"cache_rate"`
	AvgDuration  int64   `json:"avg_duration_ms"`
}

// query returns a filtered, newest-first page of the log plus totals.
func (t *telemetry) query(limit, offset int, search, status string) ([]RequestLog, int, logSummary) {
	if t == nil {
		return []RequestLog{}, 0, logSummary{}
	}
	if limit < 1 || limit > 200 {
		limit = 50
	}
	search = strings.ToLower(strings.TrimSpace(search))
	status = strings.TrimSpace(status)

	t.mu.Lock()
	all := make([]RequestLog, len(t.logs))
	copy(all, t.logs)
	t.mu.Unlock()

	filtered := make([]RequestLog, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		v := all[i]
		if search != "" && !strings.Contains(strings.ToLower(v.Model+" "+v.UpstreamModel+" "+v.SourceFormat+" "+v.Error+" "+v.AuthID), search) {
			continue
		}
		if status != "" && status != "all" {
			switch status {
			case "success":
				if v.Status >= 400 || v.Status == 0 {
					continue
				}
			case "error":
				if v.Status < 400 && v.Status != 0 {
					continue
				}
			default:
				if n, err := strconv.Atoi(status); err == nil && n != v.Status {
					continue
				}
			}
		}
		filtered = append(filtered, v)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Time.After(filtered[j].Time) })

	var sum logSummary
	var durationTotal int64
	for _, v := range filtered {
		sum.Requests++
		if v.Status >= 400 || v.Status == 0 {
			sum.Failed++
		} else {
			sum.Success++
		}
		sum.InputTokens += v.InputTokens
		sum.OutputTokens += v.OutputTokens
		sum.CachedTokens += v.CachedTokens
		durationTotal += v.DurationMS
	}
	if sum.InputTokens > 0 {
		sum.CacheRate = float64(sum.CachedTokens) / float64(sum.InputTokens)
	}
	if sum.Requests > 0 {
		sum.AvgDuration = durationTotal / int64(sum.Requests)
	}

	total := len(filtered)
	if offset < 0 {
		offset = 0
	}
	if offset >= total {
		offset = 0
		if total > 0 {
			offset = ((total - 1) / limit) * limit
		}
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return filtered[offset:end], total, sum
}

// modelStat aggregates one model's requests for the statistics tab.
type modelStat struct {
	Model        string `json:"model"`
	Requests     int    `json:"requests"`
	Success      int    `json:"success"`
	Failed       int    `json:"failed"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	AvgDuration  int64  `json:"avg_duration_ms"`
}

// stats returns the overall summary plus a per-model breakdown, newest model
// first by request volume.
func (t *telemetry) stats() (logSummary, []modelStat) {
	if t == nil {
		return logSummary{}, []modelStat{}
	}
	t.mu.Lock()
	all := make([]RequestLog, len(t.logs))
	copy(all, t.logs)
	t.mu.Unlock()

	var sum logSummary
	var durationTotal int64
	byModel := map[string]*modelStat{}
	order := []string{}
	for _, v := range all {
		sum.Requests++
		durationTotal += v.DurationMS
		if v.Status >= 400 || v.Status == 0 {
			sum.Failed++
		} else {
			sum.Success++
		}
		sum.InputTokens += v.InputTokens
		sum.OutputTokens += v.OutputTokens
		sum.CachedTokens += v.CachedTokens
		key := v.Model
		if key == "" {
			key = v.UpstreamModel
		}
		m := byModel[key]
		if m == nil {
			m = &modelStat{Model: key}
			byModel[key] = m
			order = append(order, key)
		}
		m.Requests++
		m.InputTokens += v.InputTokens
		m.OutputTokens += v.OutputTokens
		m.CachedTokens += v.CachedTokens
		m.AvgDuration += v.DurationMS
		if v.Status >= 400 || v.Status == 0 {
			m.Failed++
		} else {
			m.Success++
		}
	}
	if sum.InputTokens > 0 {
		sum.CacheRate = float64(sum.CachedTokens) / float64(sum.InputTokens)
	}
	if sum.Requests > 0 {
		sum.AvgDuration = durationTotal / int64(sum.Requests)
	}
	out := make([]modelStat, 0, len(order))
	for _, key := range order {
		m := byModel[key]
		if m.Requests > 0 {
			m.AvgDuration /= int64(m.Requests)
		}
		out = append(out, *m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Requests > out[j].Requests })
	return sum, out
}
