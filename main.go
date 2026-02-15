package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ─── Configuration ──────────────────────────────────────────

const (
	apiURL            = "https://agsi.gie.eu/api"
	country           = "DE"
	winterStartMD     = "11-01"
	targetEndMD       = "03-31"
	criticalThreshold = 10.0
	trendWindow       = 14
	stressMultiplier  = 1.25
	defaultPort       = "8080"
	fetchSize         = 300
	fetchTimeout      = 30 * time.Second
	retryAttempts     = 3
	retryDelay        = 2 * time.Second
	delayBetweenCalls = 1 * time.Second
	shutdownTimeout   = 5 * time.Second
)

// ─── Data Models ────────────────────────────────────────────

type APIResponse struct {
	Data []APIRecord `json:"data"`
}

type APIRecord struct {
	GasDayStart string `json:"gasDayStart"`
	Full        string `json:"full"`
	Injection   string `json:"injection"`
	Withdrawal  string `json:"withdrawal"`
}

type DayRecord struct {
	Date        time.Time `json:"date"`
	DateStr     string    `json:"dateStr"`
	Full        float64   `json:"full"`
	Injection   float64   `json:"injection"`
	Withdrawal  float64   `json:"withdrawal"`
	DaysElapsed int       `json:"daysElapsed"`
	Trend       float64   `json:"trend"`
	TrendMA7    float64   `json:"trendMa7"`
}

type SeasonConfig struct {
	Year      int    `json:"year"`
	Name      string `json:"name"`
	Color     string `json:"color"`
	Width     int    `json:"width"`
	Dash      string `json:"dash"`
	FillColor string `json:"fillColor"`
	IsCurrent bool   `json:"isCurrent"`
}

type SeasonData struct {
	Config  SeasonConfig `json:"config"`
	Records []DayRecord  `json:"records"`
}

type ScenarioPoint struct {
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	HoverDate string  `json:"hoverDate"`
}

type Scenario struct {
	Name     string          `json:"name"`
	Label    string          `json:"label"`
	Color    string          `json:"color"`
	Dash     string          `json:"dash"`
	Points   []ScenarioPoint `json:"points"`
	HitDate  string          `json:"hitDate,omitempty"`
	Slope    float64         `json:"slope,omitempty"`
	DaysLeft int             `json:"daysLeft,omitempty"`
}

type KPIData struct {
	CurrentFill   float64 `json:"currentFill"`
	CurrentDate   string  `json:"currentDate"`
	Delta7D       float64 `json:"delta7d"`
	AvgWithdrawal float64 `json:"avgWithdrawal"`
	DaysToCrit    int     `json:"daysToCrit"`
}

type DashboardData struct {
	Seasons     []SeasonData `json:"seasons"`
	Scenarios   []Scenario   `json:"scenarios"`
	KPI         KPIData      `json:"kpi"`
	TickVals    []int        `json:"tickVals"`
	TickLabels  []string     `json:"tickLabels"`
	GeneratedAt string       `json:"generatedAt"`
	CurrentYear int          `json:"currentYear"`
}

// ─── Data Cache ─────────────────────────────────────────────

type Cache struct {
	mu          sync.RWMutex
	data        *DashboardData
	lastFetched time.Time
	ttl         time.Duration
	building    sync.Mutex
}

var cache = &Cache{ttl: 2 * time.Hour}

func (c *Cache) Get() *DashboardData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.data != nil && time.Since(c.lastFetched) < c.ttl {
		return c.data
	}
	return nil
}

func (c *Cache) Set(d *DashboardData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = d
	c.lastFetched = time.Now()
}

func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = nil
}

// ─── Season Year Logic ──────────────────────────────────────

// currentWinterStartYear returns the start year of the
// winter season that is currently active.
// Winter 2025/26 starts Nov 2025, so from Nov 2025 through
// ~Mar 2026 the "current winter start year" is 2025.
func currentWinterStartYear() int {
	now := time.Now()
	year := now.Year()
	month := now.Month()

	// If we're in Jan–Oct, the winter started LAST year
	// (e.g. Feb 2026 → winter started Nov 2025 → return 2025)
	if month < 11 {
		return year - 1
	}
	// Nov or Dec → winter started this year
	return year
}

// ─── API Fetching ───────────────────────────────────────────

func fetchSeasonWithRetry(startYear int) ([]DayRecord, error) {
	var lastErr error
	for attempt := 1; attempt <= retryAttempts; attempt++ {
		records, err := fetchSeason(startYear)
		if err == nil {
			return records, nil
		}
		lastErr = err
		log.Printf("    ⚠️  Attempt %d/%d for %d failed: %v",
			attempt, retryAttempts, startYear, err)
		if attempt < retryAttempts {
			wait := retryDelay * time.Duration(attempt)
			log.Printf("    ⏳ Retrying in %v...", wait)
			time.Sleep(wait)
		}
	}
	return nil, fmt.Errorf("all %d attempts failed for %d: %w",
		retryAttempts, startYear, lastErr)
}

func fetchSeason(startYear int) ([]DayRecord, error) {
	startDate := fmt.Sprintf("%d-%s", startYear, winterStartMD)
	now := time.Now()

	cwsy := currentWinterStartYear()

	var endDate string
	if startYear == cwsy {
		// Current season → end at today
		endDate = now.Format("2006-01-02")
	} else {
		// Past season → end at March 31 of the following year
		endDate = fmt.Sprintf("%d-%s", startYear+1, targetEndMD)
	}

	// Sanity: don't fetch if start is in the future
	seasonStartParsed, _ := time.Parse("2006-01-02", startDate)
	if seasonStartParsed.After(now) {
		return nil, fmt.Errorf("season %d starts in the future (%s)", startYear, startDate)
	}

	url := fmt.Sprintf("%s?country=%s&from=%s&to=%s&size=%d",
		apiURL, country, startDate, endDate, fetchSize)

	log.Printf("  📡 Fetching %d/%02d: %s → %s",
		startYear, (startYear+1)%100, startDate, endDate)

	client := &http.Client{Timeout: fetchTimeout}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("request creation: %w", err)
	}

	req.Header.Set("User-Agent",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", "https://agsi.gie.eu/")
	req.Header.Set("Origin", "https://agsi.gie.eu")

	if apiKey := os.Getenv("AGSI_API_KEY"); apiKey != "" {
		req.Header.Set("x-key", apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	log.Printf("     HTTP %d, %d bytes", resp.StatusCode, len(body))

	if resp.StatusCode != http.StatusOK {
		preview := string(body)
		if len(preview) > 500 {
			preview = preview[:500]
		}
		return nil, fmt.Errorf("API status %d: %s", resp.StatusCode, preview)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("JSON decode: %w", err)
	}

	if len(apiResp.Data) == 0 {
		log.Printf("     ⚠️  Empty data array for %d", startYear)
		return nil, nil
	}

	log.Printf("  ✅ %d: %d raw records", startYear, len(apiResp.Data))

	// Parse records
	seasonStart, _ := time.Parse("2006-01-02", startDate)
	records := make([]DayRecord, 0, len(apiResp.Data))

	for _, r := range apiResp.Data {
		date := parseDate(r.GasDayStart)
		if date.IsZero() {
			log.Printf("     ⚠️  Skipping unparseable date: %q", r.GasDayStart)
			continue
		}

		elapsed := int(date.Sub(seasonStart).Hours() / 24)

		records = append(records, DayRecord{
			Date:        date,
			DateStr:     date.Format("02 Jan 2006"),
			Full:        parseFloat(r.Full),
			Injection:   parseFloat(r.Injection),
			Withdrawal:  parseFloat(r.Withdrawal),
			DaysElapsed: elapsed,
		})
	}

	if len(records) == 0 {
		return nil, fmt.Errorf("no valid records parsed")
	}

	// Sort ascending
	sort.Slice(records, func(i, j int) bool {
		return records[i].Date.Before(records[j].Date)
	})

	// Calculate trend + 7d MA
	for i := range records {
		if i > 0 {
			records[i].Trend = records[i].Full - records[i-1].Full
		}
		start := i - 6
		if start < 0 {
			start = 0
		}
		sum := 0.0
		for j := start; j <= i; j++ {
			sum += records[j].Trend
		}
		records[i].TrendMA7 = sum / float64(i-start+1)
	}

	// Debug: print first and last record
	log.Printf("     Range: %s (day %d, %.1f%%) → %s (day %d, %.1f%%)",
		records[0].DateStr, records[0].DaysElapsed, records[0].Full,
		records[len(records)-1].DateStr,
		records[len(records)-1].DaysElapsed,
		records[len(records)-1].Full)

	// Debug: show sample trend values
	if len(records) > 3 {
		last := records[len(records)-1]
		prev := records[len(records)-2]
		log.Printf("     Last trend: %.3f%% (%.1f%% → %.1f%%), MA7: %.3f%%",
			last.Trend, prev.Full, last.Full, last.TrendMA7)
	}

	return records, nil
}

func parseDate(s string) time.Time {
	// Try common formats
	formats := []string{
		"2006-01-02",
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		time.RFC3339,
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func parseFloat(s string) float64 {
	if s == "" || s == "-" || s == "N/A" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// ─── Sequential Fetch ───────────────────────────────────────

func fetchAllSeasons(configs []SeasonConfig) (map[int][]DayRecord, []SeasonData) {
	allSeasons := make(map[int][]DayRecord)
	var seasons []SeasonData

	for i, cfg := range configs {
		log.Printf("\n── Season %d/%d: %s ──", i+1, len(configs), cfg.Name)

		records, err := fetchSeasonWithRetry(cfg.Year)
		if err != nil {
			log.Printf("  ❌ %s: %v (skipping)", cfg.Name, err)
		} else if len(records) == 0 {
			log.Printf("  ⚠️  %s: no data (skipping)", cfg.Name)
		} else {
			log.Printf("  ✅ %s: %d records loaded", cfg.Name, len(records))
			allSeasons[cfg.Year] = records
			seasons = append(seasons, SeasonData{Config: cfg, Records: records})
		}

		if i < len(configs)-1 {
			time.Sleep(delayBetweenCalls)
		}
	}

	return allSeasons, seasons
}

// ─── Scenarios ──────────────────────────────────────────────

func generateScenarios(current []DayRecord, allSeasons map[int][]DayRecord,
	currentStartYear int) []Scenario {

	if len(current) < trendWindow {
		log.Printf("  ⚠️  Not enough data for scenarios (%d < %d)",
			len(current), trendWindow)
		return nil
	}

	lastIdx := len(current) - 1
	currentVal := current[lastIdx].Full
	currentDay := current[lastIdx].DaysElapsed
	lastDate := current[lastIdx].Date

	var scenarios []Scenario

	recentStart := max(len(current)-trendWindow, 0)
	slope, _ := linearRegression(current[recentStart:])
	log.Printf("  📈 Slope: %.4f%%/day over %d days", slope, len(current[recentStart:]))

	if slope < 0 {
		days := (criticalThreshold - currentVal) / slope
		hitDate := lastDate.Add(time.Duration(days*24) * time.Hour)
		scenarios = append(scenarios, Scenario{
			Name: "Linear", Label: "📉 Linear Trend",
			Color: "#c0392b", Dash: "dot",
			Points:   makeProjectionPoints(currentDay, currentVal, slope, days, lastDate, 50),
			HitDate:  hitDate.Format("02.01.2006"),
			Slope:    slope,
			DaysLeft: int(days),
		})
		log.Printf("  📉 Linear: ~%d days → %s", int(days), hitDate.Format("02 Jan 2006"))

		ss := slope * stressMultiplier
		sd := (criticalThreshold - currentVal) / ss
		shd := lastDate.Add(time.Duration(sd*24) * time.Hour)
		scenarios = append(scenarios, Scenario{
			Name: "Stress", Label: "❄️ Severe Winter",
			Color: "#800000", Dash: "dashdot",
			Points:   makeProjectionPoints(currentDay, currentVal, ss, sd, lastDate, 50),
			HitDate:  shd.Format("02.01.2006"),
			Slope:    ss,
			DaysLeft: int(sd),
		})
		log.Printf("  ❄️  Stress: ~%d days → %s", int(sd), shd.Format("02 Jan 2006"))
	}

	// Historical — use the season before current
	histYear := currentStartYear - 1
	if recs, ok := allSeasons[histYear]; ok && len(recs) > 0 {
		var pts []ScenarioPoint
		var base float64
		found := false
		for _, r := range recs {
			if r.DaysElapsed > currentDay {
				if !found {
					base = r.Full
					found = true
				}
				pts = append(pts, ScenarioPoint{
					X:         float64(r.DaysElapsed),
					Y:         currentVal + (r.Full - base),
					HoverDate: r.Date.Format("02 Jan"),
				})
			}
		}
		if len(pts) > 0 {
			scenarios = append(scenarios, Scenario{
				Name:  "History",
				Label: fmt.Sprintf("📅 Like %d/%02d", histYear, (histYear+1)%100),
				Color: "#d35400", Dash: "dash",
				Points: pts,
			})
			log.Printf("  📅 History: %d points from %d/%02d",
				len(pts), histYear, (histYear+1)%100)
		}
	}

	return scenarios
}

func makeProjectionPoints(startDay int, startVal, slope, totalDays float64,
	startDate time.Time, n int) []ScenarioPoint {
	pts := make([]ScenarioPoint, n)
	for i := 0; i < n; i++ {
		d := totalDays * float64(i) / float64(n-1)
		pts[i] = ScenarioPoint{
			X:         float64(startDay) + d,
			Y:         startVal + slope*d,
			HoverDate: startDate.Add(time.Duration(d*24) * time.Hour).Format("02 Jan 2006"),
		}
	}
	return pts
}

func linearRegression(records []DayRecord) (slope, intercept float64) {
	n := float64(len(records))
	if n < 2 {
		return 0, 0
	}
	var sx, sy, sxy, sx2 float64
	for _, r := range records {
		x := float64(r.DaysElapsed)
		sx += x
		sy += r.Full
		sxy += x * r.Full
		sx2 += x * x
	}
	d := n*sx2 - sx*sx
	if math.Abs(d) < 1e-10 {
		return 0, sy / n
	}
	slope = (n*sxy - sx*sy) / d
	intercept = (sy - slope*sx) / n
	return
}

// ─── KPI ────────────────────────────────────────────────────

func buildKPI(records []DayRecord, scenarios []Scenario) KPIData {
	last := records[len(records)-1]
	kpi := KPIData{
		CurrentFill: last.Full,
		CurrentDate: last.Date.Format("02 Jan 2006"),
		DaysToCrit:  999,
	}
	if len(records) >= 7 {
		kpi.Delta7D = last.Full - records[len(records)-7].Full
	}
	start := len(records) - 7
	if start < 0 {
		start = 0
	}
	sum := 0.0
	for _, r := range records[start:] {
		sum += r.Withdrawal
	}
	kpi.AvgWithdrawal = sum / float64(len(records)-start)
	for _, s := range scenarios {
		if s.Name == "Linear" && s.DaysLeft > 0 {
			kpi.DaysToCrit = s.DaysLeft
		}
	}
	return kpi
}

// ─── Ticks ──────────────────────────────────────────────────

func generateTicks(startYear int) ([]int, []string) {
	var vals []int
	var labels []string
	startStr := fmt.Sprintf("%d-%s", startYear, winterStartMD)
	start, _ := time.Parse("2006-01-02", startStr)
	for d := 0; d < 180; d += 7 {
		vals = append(vals, d)
		labels = append(labels, start.AddDate(0, 0, d).Format("02 Jan"))
	}
	return vals, labels
}

// ─── Dashboard Builder ─────────────────────────────────────

func buildDashboard() (*DashboardData, error) {
	log.Println("\n════════════════════════════════════════")
	log.Println("  📡 Building Dashboard")
	log.Println("════════════════════════════════════════")

	now := time.Now()
	cwsy := currentWinterStartYear()

	log.Printf("  📅 Today: %s", now.Format("02 Jan 2006"))
	log.Printf("  📅 Current winter start year: %d (season %d/%02d)",
		cwsy, cwsy, (cwsy+1)%100)

	// Build season configs: 2 prior + current
	configs := []SeasonConfig{
		{
			Year:  cwsy - 2,
			Name:  fmt.Sprintf("Winter %d/%02d", cwsy-2, (cwsy-1)%100),
			Color: "#95a5a6", Width: 3, Dash: "dash",
			FillColor: "rgba(149,165,166,0.08)",
			IsCurrent: false,
		},
		{
			Year:  cwsy - 1,
			Name:  fmt.Sprintf("Winter %d/%02d", cwsy-1, cwsy%100),
			Color: "#e67e22", Width: 3, Dash: "dash",
			FillColor: "rgba(230,126,34,0.10)",
			IsCurrent: false,
		},
		{
			Year:  cwsy,
			Name:  fmt.Sprintf("Winter %d/%02d (Current)", cwsy, (cwsy+1)%100),
			Color: "#004080", Width: 4, Dash: "solid",
			FillColor: "rgba(0,64,128,0.18)",
			IsCurrent: true,
		},
	}

	allSeasons, seasons := fetchAllSeasons(configs)

	if len(seasons) == 0 {
		return nil, fmt.Errorf("no season data loaded from API")
	}

	// Find the current season records
	var currentRecords []DayRecord
	var currentFound bool

	if r, ok := allSeasons[cwsy]; ok && len(r) > 0 {
		currentRecords = r
		currentFound = true
		log.Printf("\n  ✅ Current season %d: %d records", cwsy, len(r))
	}

	if !currentFound {
		// Fallback: use the most recent season with data
		for i := len(seasons) - 1; i >= 0; i-- {
			if len(seasons[i].Records) > 0 {
				currentRecords = seasons[i].Records
				// Mark it as current
				seasons[i].Config.IsCurrent = true
				log.Printf("\n  ⚠️  Fallback: using %s as current (%d records)",
					seasons[i].Config.Name, len(currentRecords))
				currentFound = true
				break
			}
		}
	}

	if !currentFound {
		return nil, fmt.Errorf("no usable current season data")
	}

	// Debug: verify trend data exists
	nonZeroTrend := 0
	for _, r := range currentRecords {
		if r.Trend != 0 {
			nonZeroTrend++
		}
	}
	log.Printf("  📊 Current season: %d records, %d with non-zero trend",
		len(currentRecords), nonZeroTrend)

	scenarios := generateScenarios(currentRecords, allSeasons, cwsy)
	kpi := buildKPI(currentRecords, scenarios)
	tv, tl := generateTicks(cwsy)

	log.Printf("\n  ✅ Dashboard built:")
	log.Printf("     Seasons   : %d", len(seasons))
	log.Printf("     Scenarios : %d", len(scenarios))
	log.Printf("     Fill      : %.1f%% as of %s", kpi.CurrentFill, kpi.CurrentDate)
	log.Printf("     7d Δ      : %.2f%%", kpi.Delta7D)
	log.Printf("     Avg withdrawal: %.0f GWh/d", kpi.AvgWithdrawal)
	if kpi.DaysToCrit < 999 {
		log.Printf("     Days to critical: ~%d", kpi.DaysToCrit)
	}

	return &DashboardData{
		Seasons:     seasons,
		Scenarios:   scenarios,
		KPI:         kpi,
		TickVals:    tv,
		TickLabels:  tl,
		GeneratedAt: now.Format("02 Jan 2006 15:04"),
		CurrentYear: cwsy,
	}, nil
}

// ─── HTTP Handlers ──────────────────────────────────────────

func handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tmpl, err := template.ParseFiles("templates/dashboard.html")
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(w, nil)
}

func handleAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if cached := cache.Get(); cached != nil {
		log.Println("📦 Serving cached data")
		json.NewEncoder(w).Encode(cached)
		return
	}

	cache.building.Lock()
	defer cache.building.Unlock()

	if cached := cache.Get(); cached != nil {
		json.NewEncoder(w).Encode(cached)
		return
	}

	data, err := buildDashboard()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{
			"error":   err.Error(),
			"message": "Failed to build dashboard.",
			"hint":    "Set AGSI_API_KEY env var if API requires auth.",
		})
		return
	}
	cache.Set(data)
	json.NewEncoder(w).Encode(data)
}

func handleRefresh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	log.Println("\n🔄 Force refresh")
	cache.Clear()

	cache.building.Lock()
	defer cache.building.Unlock()

	data, err := buildDashboard()
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	cache.Set(data)
	json.NewEncoder(w).Encode(data)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"hasData": cache.Get() != nil,
		"time":    time.Now().Format(time.RFC3339),
	})
}

// ─── Port Discovery ─────────────────────────────────────────

func findAvailablePort(preferred string) string {
	ln, err := net.Listen("tcp", ":"+preferred)
	if err == nil {
		ln.Close()
		return preferred
	}
	log.Printf("⚠️  Port %s busy: %v", preferred, err)

	for _, p := range []string{"8081", "8082", "8083", "8090", "9090"} {
		if ln, err := net.Listen("tcp", ":"+p); err == nil {
			ln.Close()
			log.Printf("✅ Using port %s", p)
			return p
		}
	}

	ln, err = net.Listen("tcp", ":0")
	if err != nil {
		log.Fatalf("❌ No available port: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	ln.Close()
	return port
}

// ─── Main ───────────────────────────────────────────────────

func main() {
	preferred := defaultPort
	if p := os.Getenv("PORT"); p != "" {
		preferred = p
	}

	port := findAvailablePort(preferred)
	addr := ":" + port

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboard)
	mux.HandleFunc("/api/data", handleAPI)
	mux.HandleFunc("/api/refresh", handleRefresh)
	mux.HandleFunc("/api/health", handleHealth)

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	cwsy := currentWinterStartYear()

	log.Println("══════════════════════════════════════════")
	log.Println("  🚀 German Gas Storage Dashboard")
	log.Println("══════════════════════════════════════════")
	log.Printf("  Dashboard:  http://localhost:%s", port)
	log.Printf("  API:        http://localhost:%s/api/data", port)
	log.Printf("  Health:     http://localhost:%s/api/health", port)
	log.Printf("  Season:     Winter %d/%02d", cwsy, (cwsy+1)%100)
	log.Println()
	if apiKey := os.Getenv("AGSI_API_KEY"); apiKey != "" {
		log.Println("  🔑 API Key: configured")
	} else {
		log.Println("  ⚠️  No API key. Set AGSI_API_KEY if needed.")
	}
	log.Println()
	log.Println("  Press Ctrl+C to stop")
	log.Println("══════════════════════════════════════════")

	// Pre-fetch
	go func() {
		log.Println("\n🔄 Pre-fetching...")
		cache.building.Lock()
		defer cache.building.Unlock()

		data, err := buildDashboard()
		if err != nil {
			log.Printf("⚠️  Pre-fetch failed: %v", err)
		} else {
			cache.Set(data)
			log.Println("✅ Ready!")
		}
	}()

	// Graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-stop
		log.Println("\n🛑 Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("❌ Shutdown error: %v", err)
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("❌ Server failed: %v", err)
	}
	log.Println("👋 Bye!")
}
