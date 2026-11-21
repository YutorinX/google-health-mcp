package health_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shmsi/google-health-mcp/internal/health"
	"github.com/shmsi/google-health-mcp/internal/testapi"
)

func d(s string) health.Date {
	t, _ := time.Parse("2006-01-02", s)
	return health.DateOf(t)
}

func TestListUsesReconcileAndCivilFilter(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) { return 200, testapi.Points("sleep", 2, "") })
	c := health.NewClient(api.Client(), api.URL)

	res, err := c.ListDataPoints(context.Background(), health.ListOptions{
		DataType: "sleep", Start: d("2026-11-19"), End: d("2026-11-21"), Reconcile: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := api.Calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	got := calls[0]
	if got.Path != "/users/me/dataTypes/sleep/dataPoints:reconcile" {
		t.Errorf("path = %s", got.Path)
	}
	want := `sleep.interval.civil_end_time >= "2026-11-19" AND sleep.interval.civil_end_time < "2026-11-21"`
	if got.Query["filter"] != want {
		t.Errorf("filter = %s", got.Query["filter"])
	}
	if got.Query["pageSize"] != "25" {
		t.Errorf("pageSize = %s, want 25 for sleep", got.Query["pageSize"])
	}
	if res.Count != 2 || res.Method != "reconcile" {
		t.Errorf("res = %+v", res)
	}
}

func TestListFallsBackToOtherFilterKinds(t *testing.T) {
	// Pretend weight is actually filtered on interval, not sample_time.
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		if strings.Contains(r.Query["filter"], "interval.civil_start_time") {
			return 200, testapi.Points("weight", 1, "")
		}
		return 400, testapi.Error(400, "INVALID_ARGUMENT", "unknown field", "")
	})
	c := health.NewClient(api.Client(), api.URL)
	res, err := c.ListDataPoints(context.Background(), health.ListOptions{DataType: "weight", Start: d("2026-11-01"), End: d("2026-11-02")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Filter, "weight.interval.civil_start_time") {
		t.Errorf("filter = %s", res.Filter)
	}
	if n := len(api.Calls()); n != 2 {
		t.Errorf("calls = %d, want 2 (sample then interval)", n)
	}
}

func TestListDoesNotRetryOtherErrors(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		return 400, testapi.Error(400, "FAILED_PRECONDITION", "The account is not linked to Google Health.", "ACCOUNT_NOT_LINKED")
	})
	c := health.NewClient(api.Client(), api.URL)
	_, err := c.ListDataPoints(context.Background(), health.ListOptions{DataType: "steps", Start: d("2026-11-01"), End: d("2026-11-02")})
	var ae *health.APIError
	if !errors.As(err, &ae) || ae.Reason != "ACCOUNT_NOT_LINKED" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "fitbit.google.com") {
		t.Errorf("error lacks hint: %v", err)
	}
	if n := len(api.Calls()); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestListPaginatesAndStopsAtMax(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		switch r.Query["pageToken"] {
		case "":
			return 200, testapi.Points("steps", 3, "p2")
		case "p2":
			return 200, testapi.Points("steps", 3, "p3")
		}
		return 200, testapi.Points("steps", 3, "")
	})
	c := health.NewClient(api.Client(), api.URL)
	res, err := c.ListDataPoints(context.Background(), health.ListOptions{DataType: "steps", Start: d("2026-11-01"), End: d("2026-11-02"), MaxPoints: 6})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 6 || !res.Truncated || res.NextPageToken != "p3" {
		t.Errorf("count=%d truncated=%v next=%q", res.Count, res.Truncated, res.NextPageToken)
	}
	calls := api.Calls()
	if calls[1].Query["filter"] != calls[0].Query["filter"] {
		t.Error("second page should reuse the filter")
	}
}

func TestListRejectsAggregateOnlyType(t *testing.T) {
	c := health.NewClient(http.DefaultClient, "http://unused")
	_, err := c.ListDataPoints(context.Background(), health.ListOptions{DataType: "total-calories", Start: d("2026-11-01"), End: d("2026-11-02")})
	if err == nil || !strings.Contains(err.Error(), "can't be listed") {
		t.Fatalf("err = %v", err)
	}
}

func TestDailyRollUpSplitsLongRanges(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		return 200, `{"rollupDataPoints":[{"steps":{"countSum":"100"}}]}`
	})
	c := health.NewClient(api.Client(), api.URL)
	// heart-rate allows 14 days per request: 30 days -> 14 + 14 + 2.
	res, err := c.DailyRollUp(context.Background(), "heart-rate", d("2026-10-01"), d("2026-10-31"), 1, "google-wearables")
	if err != nil {
		t.Fatal(err)
	}
	calls := api.Calls()
	if len(calls) != 3 || res.Count != 3 {
		t.Fatalf("calls=%d count=%d", len(calls), res.Count)
	}
	first := calls[0]
	if first.Method != "POST" || first.Path != "/users/me/dataTypes/heart-rate/dataPoints:dailyRollUp" {
		t.Errorf("%s %s", first.Method, first.Path)
	}
	rng := first.Body["range"].(map[string]any)
	start := rng["start"].(map[string]any)["date"].(map[string]any)
	end := rng["end"].(map[string]any)["date"].(map[string]any)
	if start["day"].(float64) != 1 || end["day"].(float64) != 15 {
		t.Errorf("first chunk = %v .. %v", start, end)
	}
	if first.Body["dataSourceFamily"] != "users/me/dataSourceFamilies/google-wearables" {
		t.Errorf("family = %v", first.Body["dataSourceFamily"])
	}
	lastEnd := calls[2].Body["range"].(map[string]any)["end"].(map[string]any)["date"].(map[string]any)
	if lastEnd["day"].(float64) != 31 {
		t.Errorf("last chunk end = %v", lastEnd)
	}
}

func TestDailyRollUpFollowsPageTokens(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		if r.Body["pageToken"] == nil {
			return 200, `{"rollupDataPoints":[{}],"nextPageToken":"t2"}`
		}
		return 200, `{"rollupDataPoints":[{}]}`
	})
	c := health.NewClient(api.Client(), api.URL)
	res, err := c.DailyRollUp(context.Background(), "steps", d("2026-11-01"), d("2026-11-03"), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 {
		t.Errorf("count = %d", res.Count)
	}
	if _, ok := api.Calls()[0].Body["dataSourceFamily"]; ok {
		t.Error("default family should be omitted")
	}
}

func TestRollUpBody(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		return 200, `{"rollupDataPoints":[{},{},{}]}`
	})
	c := health.NewClient(api.Client(), api.URL)
	loc, _ := time.LoadLocation("Europe/Stockholm")
	start := time.Date(2026, 11, 20, 6, 0, 0, 0, loc)
	res, err := c.RollUp(context.Background(), "heart-rate", start, start.Add(3*time.Hour), time.Hour, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	b := api.Calls()[0].Body
	if b["windowSize"] != "3600s" {
		t.Errorf("windowSize = %v", b["windowSize"])
	}
	rng := b["range"].(map[string]any)
	if rng["startTime"] != "2026-11-20T05:00:00Z" || rng["endTime"] != "2026-11-20T08:00:00Z" {
		t.Errorf("range = %v", rng)
	}
	if res.Count != 3 {
		t.Errorf("count = %d", res.Count)
	}
}

func TestRollUpMaxPoints(t *testing.T) {
	api := testapi.New(t, func(r testapi.Request) (int, string) {
		return 200, `{"rollupDataPoints":[{},{},{},{},{}]}`
	})
	c := health.NewClient(api.Client(), api.URL)
	start := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	res, err := c.RollUp(context.Background(), "steps", start, start.Add(5*time.Hour), time.Hour, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 || !res.Truncated {
		t.Errorf("count=%d truncated=%v", res.Count, res.Truncated)
	}
}

func TestParseDate(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Stockholm")
	// 23:30 UTC on the 19th is already the 20th in Stockholm.
	now := time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC)
	for in, want := range map[string]string{
		"today": "2026-11-20", "yesterday": "2026-11-19", "-7d": "2026-11-13", "2026-01-05": "2026-01-05",
	} {
		got, err := health.ParseDate(in, now, loc)
		if err != nil || got.String() != want {
			t.Errorf("ParseDate(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	if _, err := health.ParseDate("next tuesday", now, loc); err == nil {
		t.Error("expected error")
	}
	got, err := health.ParseInstant("2026-11-20T07:30", now, loc)
	if err != nil || got.UTC().Format(time.RFC3339) != "2026-11-20T06:30:00Z" {
		t.Errorf("ParseInstant local = %v, %v", got, err)
	}
}

func TestCatalogFilterKinds(t *testing.T) {
	for id, want := range map[string]string{
		"steps":                    `steps.interval.civil_start_time >= "2026-11-01"`,
		"heart-rate":               `heart_rate.sample_time.civil_time >= "2026-11-01"`,
		"daily-resting-heart-rate": `daily_resting_heart_rate.date >= "2026-11-01"`,
	} {
		api := testapi.New(t, func(r testapi.Request) (int, string) { return 200, `{"dataPoints":[]}` })
		c := health.NewClient(api.Client(), api.URL)
		res, err := c.ListDataPoints(context.Background(), health.ListOptions{DataType: id, Start: d("2026-11-01"), End: d("2026-11-02")})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Filter, want) {
			t.Errorf("%s filter = %s", id, res.Filter)
		}
	}
}
