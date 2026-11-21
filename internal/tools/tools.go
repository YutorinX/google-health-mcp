// Package tools registers the MCP tools that expose the Google Health API.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shmsi/google-health-mcp/internal/health"
)

// Deps are what the tools need.
type Deps struct {
	Client   *health.Client
	Location *time.Location   // user's time zone for "today" and local times
	Now      func() time.Time // injectable for tests
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func textResult(v any) (*mcp.CallToolResult, any, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep filters like >= readable
	if err := enc.Encode(v); err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimSpace(b.String())}}}, nil, nil
}

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(true)}

func ptr[T any](v T) *T { return &v }

// Instructions are sent to the client when it connects.
const Instructions = "Read-only access to the user's Google Health data (Fitbit / Pixel Watch): sleep, activity, heart rate, HRV, SpO2, weight and more. " +
	"Use get_daily_overview for a quick picture of recent days, get_daily_totals for trends, get_data_points for sleep/workout details, " +
	"and get_rollup for intraday heart rate or steps. Health data is personal: only fetch what the user's question needs."

// NewServer returns an MCP server with all tools registered. cache may be
// nil; pass one when creating a server per request.
func NewServer(d Deps, version string, cache *mcp.SchemaCache) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "google-health", Title: "Google Health", Version: version},
		&mcp.ServerOptions{Instructions: Instructions, SchemaCache: cache})
	Register(s, d)
	return s
}

const dateHelp = "YYYY-MM-DD, 'today', 'yesterday' or a relative offset like '-7d'"

// Register adds all tools to the server.
func Register(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_data_types",
		Title:       "List health data types",
		Annotations: readOnly,
		Description: "Lists the Google Health data types this server can read (steps, sleep, heart-rate, daily-resting-heart-rate, …) " +
			"with which tools work for each: list → get_data_points, dailyRollUp → get_daily_totals, rollUp → get_rollup.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return textResult(health.SortedCatalog())
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_account_info",
		Title:       "Account, devices and today's date",
		Annotations: readOnly,
		Description: "Checks the Google Health connection and returns the user's identity, profile, settings, paired devices " +
			"(e.g. Pixel Watch, with battery and last sync), plus the server's current local date and time zone. Call this first if unsure what 'today' is.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		now := d.now().In(d.Location)
		out := map[string]any{
			"localNow": now.Format(time.RFC3339),
			"timeZone": d.Location.String(),
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for _, r := range []string{"identity", "profile", "settings", "pairedDevices"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				v, err := d.Client.Get(ctx, r)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					out[r] = map[string]string{"error": err.Error()}
				} else {
					out[r] = v
				}
			}()
		}
		wg.Wait()
		return textResult(out)
	})

	type dataPointsIn struct {
		DataType         string `json:"data_type" jsonschema:"data type id, e.g. sleep, exercise, daily-resting-heart-rate, weight (see list_data_types)"`
		StartDate        string `json:"start_date" jsonschema:"first day to include: YYYY-MM-DD, 'today', 'yesterday' or '-7d'"`
		EndDate          string `json:"end_date,omitempty" jsonschema:"last day to include (inclusive); defaults to start_date"`
		Reconciled       *bool  `json:"reconciled,omitempty" jsonschema:"use the reconciled stream that merges overlapping sources like the Fitbit app does (default true where supported)"`
		DataSourceFamily string `json:"data_source_family,omitempty" jsonschema:"all-sources (default), google-wearables, google-sources or self-sources"`
		MaxPoints        int    `json:"max_points,omitempty" jsonschema:"maximum data points to return (default 200, max 2000)"`
		PageToken        string `json:"page_token,omitempty" jsonschema:"nextPageToken from a previous truncated call to continue"`
		Filter           string `json:"filter,omitempty" jsonschema:"advanced: raw AIP-160 filter; replaces the date range"`
		Verbose          bool   `json:"verbose,omitempty" jsonschema:"keep each point's name and dataSource fields (dropped by default to save space)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_data_points",
		Title:       "Get raw data points",
		Annotations: readOnly,
		Description: "Returns individual Google Health data points for a range of days, newest first. Best for sleep sessions (with stages), " +
			"exercise sessions, daily-* summaries (resting heart rate, HRV, SpO2, breathing rate, skin temperature), weight and other sparse measurements. " +
			"Sleep is matched by the day the sleep ENDED, so last night's sleep is under today's date. " +
			"Dense types like heart-rate or steps produce huge outputs — use get_daily_totals or get_rollup for those. Dates: " + dateHelp + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dataPointsIn) (*mcp.CallToolResult, any, error) {
		start, end, err := dayRange(in.StartDate, in.EndDate, d, 366)
		if err != nil {
			return nil, nil, err
		}
		reconcile := in.Reconciled == nil || *in.Reconciled
		if in.MaxPoints > 2000 {
			in.MaxPoints = 2000
		}
		res, err := d.Client.ListDataPoints(ctx, health.ListOptions{
			DataType: in.DataType, Start: start, End: end, Reconcile: reconcile,
			Family: in.DataSourceFamily, Filter: in.Filter, PageToken: in.PageToken, MaxPoints: in.MaxPoints,
		})
		if err != nil {
			return nil, nil, err
		}
		if !in.Verbose {
			for i, p := range res.DataPoints {
				res.DataPoints[i] = stripFields(p, "name", "dataSource", "dataPointName")
			}
		}
		out := map[string]any{"result": res, "range": map[string]string{"start": start.String(), "endInclusive": end.AddDays(-1).String()}}
		if res.Truncated {
			if res.NextPageToken != "" {
				out["note"] = "More data exists: call again with page_token, or narrow the date range."
			} else {
				out["note"] = "Output was cut to stay small: narrow the date range or use get_daily_totals / get_rollup."
			}
		}
		return textResult(out)
	})

	type dailyIn struct {
		DataType         string `json:"data_type" jsonschema:"data type id, e.g. steps, distance, floors, active-zone-minutes, total-calories, heart-rate, weight"`
		StartDate        string `json:"start_date" jsonschema:"first day to include: YYYY-MM-DD, 'today', 'yesterday' or '-30d'"`
		EndDate          string `json:"end_date,omitempty" jsonschema:"last day to include (inclusive); defaults to start_date"`
		WindowDays       int    `json:"window_days,omitempty" jsonschema:"days per bucket: 1 = per day (default), 7 = weekly"`
		DataSourceFamily string `json:"data_source_family,omitempty" jsonschema:"all-sources (default), google-wearables, google-sources or self-sources"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_daily_totals",
		Title:       "Daily totals / aggregates",
		Annotations: readOnly,
		Description: "Aggregates a data type per calendar day in the user's local time (or per N-day bucket): total steps, distance, floors, " +
			"calories, active zone minutes, min/max/avg heart rate, average weight, etc. Up to 366 days; long ranges are split automatically. Dates: " + dateHelp + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dailyIn) (*mcp.CallToolResult, any, error) {
		start, end, err := dayRange(in.StartDate, in.EndDate, d, 366)
		if err != nil {
			return nil, nil, err
		}
		res, err := d.Client.DailyRollUp(ctx, in.DataType, start, end, in.WindowDays, in.DataSourceFamily)
		if err != nil {
			return nil, nil, err
		}
		return textResult(res)
	})

	type rollupIn struct {
		DataType         string `json:"data_type" jsonschema:"data type id, e.g. heart-rate, steps, active-zone-minutes"`
		Start            string `json:"start" jsonschema:"range start: RFC 3339 timestamp, local 'YYYY-MM-DDTHH:MM', or a date (local midnight)"`
		End              string `json:"end,omitempty" jsonschema:"range end (exclusive), same formats or 'now'; defaults to 24h after start"`
		Window           string `json:"window" jsonschema:"bucket size as a Go duration, e.g. 5m, 15m, 1h"`
		DataSourceFamily string `json:"data_source_family,omitempty" jsonschema:"all-sources (default), google-wearables, google-sources or self-sources"`
		MaxPoints        int    `json:"max_points,omitempty" jsonschema:"maximum buckets to return (default 500, max 3000)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_rollup",
		Title:       "Intraday rollup",
		Annotations: readOnly,
		Description: "Aggregates a data type into fixed time buckets (e.g. heart rate every 15 minutes during a workout, steps per hour). " +
			"Use this instead of raw data points for dense intraday data. Times without an offset are in the user's local time zone.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in rollupIn) (*mcp.CallToolResult, any, error) {
		now := d.now()
		start, err := health.ParseInstant(in.Start, now, d.Location)
		if err != nil {
			return nil, nil, err
		}
		end := start.Add(24 * time.Hour)
		if in.End != "" {
			if end, err = health.ParseInstant(in.End, now, d.Location); err != nil {
				return nil, nil, err
			}
		}
		window, err := time.ParseDuration(strings.TrimSpace(in.Window))
		if err != nil {
			return nil, nil, fmt.Errorf("invalid window %q: use a duration like 15m or 1h", in.Window)
		}
		if n := end.Sub(start) / window; n > 3000 {
			return nil, nil, fmt.Errorf("that would be %d buckets; use a larger window or a shorter range (max 3000)", n)
		}
		if in.MaxPoints > 3000 {
			in.MaxPoints = 3000
		}
		res, err := d.Client.RollUp(ctx, in.DataType, start, end, window, in.DataSourceFamily, in.MaxPoints)
		if err != nil {
			return nil, nil, err
		}
		return textResult(res)
	})

	type overviewIn struct {
		StartDate string `json:"start_date" jsonschema:"first day: YYYY-MM-DD, 'today', 'yesterday' or '-7d'"`
		EndDate   string `json:"end_date,omitempty" jsonschema:"last day (inclusive, max 31 days); defaults to start_date"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_daily_overview",
		Title:       "Daily health overview",
		Annotations: readOnly,
		Description: "One call for a broad picture of one or more days (max 31): daily steps, distance, floors, active zone minutes and calories; " +
			"resting heart rate, HRV, SpO2, breathing rate; sleep sessions (by the day sleep ended) and workouts. " +
			"Sections the account has no data or permission for show an error instead. Dates: " + dateHelp + ".",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in overviewIn) (*mcp.CallToolResult, any, error) {
		start, end, err := dayRange(in.StartDate, in.EndDate, d, 31)
		if err != nil {
			return nil, nil, err
		}
		return textResult(overview(ctx, d.Client, start, end))
	})
}

// overview fetches several data types concurrently; failures are reported
// per section.
func overview(ctx context.Context, c *health.Client, start, end health.Date) map[string]any {
	out := map[string]any{"range": map[string]string{"start": start.String(), "endInclusive": end.AddDays(-1).String()}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	set := func(k string, v any, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			out[k] = map[string]string{"error": err.Error()}
		} else {
			out[k] = v
		}
	}
	for _, t := range []string{"steps", "distance", "floors", "active-zone-minutes", "total-calories"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.DailyRollUp(ctx, t, start, end, 1, "")
			if err == nil {
				set(t, r.Points, nil)
			} else {
				set(t, nil, err)
			}
		}()
	}
	for _, t := range []string{"daily-resting-heart-rate", "daily-heart-rate-variability", "daily-oxygen-saturation", "daily-respiratory-rate", "sleep", "exercise"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := c.ListDataPoints(ctx, health.ListOptions{DataType: t, Start: start, End: end, Reconcile: true, MaxPoints: 100, MaxBytes: 60_000})
			if err != nil {
				set(t, nil, err)
				return
			}
			for i, p := range r.DataPoints {
				r.DataPoints[i] = stripFields(p, "name", "dataSource", "dataPointName")
			}
			set(t, r.DataPoints, nil)
		}()
	}
	wg.Wait()
	return out
}

// dayRange parses an inclusive [start, end] day range and returns it as
// [start, end+1).
func dayRange(startS, endS string, d Deps, maxDays int) (health.Date, health.Date, error) {
	now := d.now()
	start, err := health.ParseDate(startS, now, d.Location)
	if err != nil {
		return health.Date{}, health.Date{}, err
	}
	last := start
	if strings.TrimSpace(endS) != "" {
		if last, err = health.ParseDate(endS, now, d.Location); err != nil {
			return health.Date{}, health.Date{}, err
		}
	}
	if last.Before(start) {
		return health.Date{}, health.Date{}, fmt.Errorf("end_date %s is before start_date %s", last, start)
	}
	end := last.AddDays(1)
	if n := start.DaysUntil(end); n > maxDays {
		return health.Date{}, health.Date{}, fmt.Errorf("range is %d days; the maximum here is %d", n, maxDays)
	}
	return start, end, nil
}

// stripFields removes top-level keys from a JSON object, returning the
// input unchanged if it isn't one.
func stripFields(raw json.RawMessage, keys ...string) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	for _, k := range keys {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}
