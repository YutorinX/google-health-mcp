package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shmsi/google-health-mcp/internal/health"
	"github.com/shmsi/google-health-mcp/internal/testapi"
	"github.com/shmsi/google-health-mcp/internal/tools"
)

func connect(t *testing.T, api *testapi.Server) *mcp.ClientSession {
	t.Helper()
	loc, _ := time.LoadLocation("Europe/Stockholm")
	s := mcp.NewServer(&mcp.Implementation{Name: "google-health", Version: "test"}, nil)
	tools.Register(s, tools.Deps{
		Client:   health.NewClient(api.Client(), api.URL),
		Location: loc,
		Now:      func() time.Time { return time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC) },
	})
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := c.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestToolsAreListedReadOnly(t *testing.T) {
	cs := connect(t, testapi.New(t, func(testapi.Request) (int, string) { return 200, "{}" }))
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"list_data_types": true, "get_account_info": true, "get_data_points": true, "get_daily_totals": true, "get_rollup": true, "get_daily_overview": true}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %s", tool.Name)
		}
		delete(want, tool.Name)
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s not marked read-only", tool.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing tools: %v", want)
	}
}

func TestGetDataPointsSleepLastNight(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) { return 200, testapi.Points("sleep", 1, "") })
	cs := connect(t, api)
	text, isErr := call(t, cs, "get_data_points", map[string]any{"data_type": "sleep", "start_date": "today"})
	if isErr {
		t.Fatal(text)
	}
	f := api.Calls()[0].Query["filter"]
	if !strings.Contains(f, `>= "2026-11-20"`) || !strings.Contains(f, `< "2026-11-21"`) {
		t.Errorf("filter = %s", f)
	}
	if strings.Contains(text, "dataSource") || strings.Contains(text, `"name"`) {
		t.Errorf("metadata not stripped: %s", text)
	}
	var out struct {
		Result health.ListResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Result.Count != 1 {
		t.Errorf("bad output %s (%v)", text, err)
	}
}

func TestGetDataPointsBadInputIsToolError(t *testing.T) {
	cs := connect(t, testapi.New(t, func(testapi.Request) (int, string) { return 200, "{}" }))
	text, isErr := call(t, cs, "get_data_points", map[string]any{"data_type": "nope", "start_date": "today"})
	if !isErr || !strings.Contains(text, "unknown data type") {
		t.Errorf("got %v %s", isErr, text)
	}
	text, isErr = call(t, cs, "get_data_points", map[string]any{"data_type": "steps", "start_date": "2026-11-20", "end_date": "2026-11-01"})
	if !isErr || !strings.Contains(text, "before") {
		t.Errorf("got %v %s", isErr, text)
	}
}

func TestDailyTotalsWeekly(t *testing.T) {
	api := testapi.New(t, func(testapi.Request) (int, string) {
		return 200, `{"rollupDataPoints":[{"steps":{"countSum":"52000"}}]}`
	})
	cs := connect(t, api)
	text, isErr := call(t, cs, "get_daily_totals", map[string]any{"data_type": "steps", "start_date": "-27d", "end_date": "today", "window_days": 7})
	if isErr {
		t.Fatal(text)
	}
	b := api.Calls()[0].Body
	if b["windowSizeDays"].(float64) != 7 {
		t.Errorf("windowSizeDays = %v", b["windowSizeDays"])
	}
	if !strings.Contains(text, "52000") {
		t.Errorf("output %s", text)
	}
}

func TestRollupLocalTimes(t *testing.T) {
	api := testapi.New(t, func(testapi.Request) (int, string) { return 200, `{"rollupDataPoints":[]}` })
	cs := connect(t, api)
	text, isErr := call(t, cs, "get_rollup", map[string]any{"data_type": "heart-rate", "start": "2026-11-20T07:00", "end": "2026-11-20T08:00", "window": "5m"})
	if isErr {
		t.Fatal(text)
	}
	b := api.Calls()[0].Body
	if b["windowSize"] != "300s" || b["range"].(map[string]any)["startTime"] != "2026-11-20T06:00:00Z" {
		t.Errorf("body = %v", b)
	}
	_, isErr = call(t, cs, "get_rollup", map[string]any{"data_type": "heart-rate", "start": "2026-11-01", "end": "2026-11-14", "window": "1s"})
	if !isErr {
		t.Error("expected too-many-buckets error")
	}
}

func TestOverviewReportsPartialFailures(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		if strings.Contains(r.Path, "daily-oxygen-saturation") {
			return 403, testapi.Error(403, "PERMISSION_DENIED", "Request had insufficient authentication scopes.", "")
		}
		if strings.HasSuffix(r.Path, ":dailyRollUp") {
			return 200, `{"rollupDataPoints":[{"x":{}}]}`
		}
		return 200, testapi.Points("x", 1, "")
	})
	cs := connect(t, api)
	text, isErr := call(t, cs, "get_daily_overview", map[string]any{"start_date": "yesterday"})
	if isErr {
		t.Fatal(text)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out["daily-oxygen-saturation"]), "PERMISSION_DENIED") {
		t.Errorf("spo2 = %s", out["daily-oxygen-saturation"])
	}
	for _, k := range []string{"steps", "sleep", "exercise", "daily-resting-heart-rate"} {
		if strings.Contains(string(out[k]), "error") || len(out[k]) == 0 {
			t.Errorf("%s = %s", k, out[k])
		}
	}
	if n := len(api.Calls()); n != 11 {
		t.Errorf("calls = %d, want 11", n)
	}
}

func TestAccountInfo(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		if strings.HasSuffix(r.Path, "/identity") {
			return 200, `{"name":"users/me/identity","healthUserId":"123"}`
		}
		return 200, `{}`
	})
	cs := connect(t, api)
	text, isErr := call(t, cs, "get_account_info", nil)
	if isErr || !strings.Contains(text, `"healthUserId":"123"`) || !strings.Contains(text, "Europe/Stockholm") || !strings.Contains(text, "2026-11-20T16:00:00+01:00") {
		t.Errorf("got %v %s", isErr, text)
	}
}
