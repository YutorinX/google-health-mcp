// Package health is a small client for the read side of the Google Health
// API (https://developers.google.com/health), the successor to the Fitbit
// Web API.
package health

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://health.googleapis.com/v4"

// Client calls the Google Health API. HTTP must add OAuth credentials
// (see oauth2.NewClient).
type Client struct {
	HTTP    *http.Client
	BaseURL string // e.g. https://health.googleapis.com/v4
}

func NewClient(httpClient *http.Client, baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{HTTP: httpClient, BaseURL: strings.TrimRight(baseURL, "/")}
}

// APIError is a non-2xx response from the API.
type APIError struct {
	HTTPStatus int
	Status     string // e.g. INVALID_ARGUMENT
	Message    string
	Reason     string // ErrorInfo.reason, e.g. ACCOUNT_NOT_LINKED
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("Google Health API error %d", e.HTTPStatus)
	if e.Status != "" {
		msg += " " + e.Status
	}
	if e.Reason != "" {
		msg += " (" + e.Reason + ")"
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	switch {
	case e.Reason == "ACCOUNT_NOT_LINKED":
		msg += ". Your Google Account isn't linked to Google Health/Fitbit yet: open the Fitbit app (or https://fitbit.google.com/auth/signup) and finish moving your Fitbit account to Google."
	case e.HTTPStatus == http.StatusForbidden:
		msg += ". The login may be missing the scope for this data type: run `google-health-mcp login` again and tick every permission."
	}
	return msg
}

func isInvalidArgument(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.HTTPStatus == http.StatusBadRequest && ae.Reason != "ACCOUNT_NOT_LINKED"
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any) (json.RawMessage, error) {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, parseAPIError(resp.StatusCode, data)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data = []byte("{}")
	}
	return data, nil
}

func parseAPIError(code int, data []byte) error {
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		} `json:"error"`
	}
	ae := &APIError{HTTPStatus: code}
	if json.Unmarshal(data, &env) == nil && (env.Error.Message != "" || env.Error.Status != "") {
		ae.Message, ae.Status = env.Error.Message, env.Error.Status
		for _, d := range env.Error.Details {
			if d.Reason != "" {
				ae.Reason = d.Reason
				break
			}
		}
	} else {
		ae.Message = strings.TrimSpace(string(data))
		if len(ae.Message) > 500 {
			ae.Message = ae.Message[:500] + "…"
		}
	}
	return ae
}

// Get fetches a users/me resource such as "identity", "profile",
// "settings" or "pairedDevices".
func (c *Client) Get(ctx context.Context, resource string) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/users/me/"+resource, nil, nil)
}

func familyName(f string) string {
	f = strings.TrimSpace(f)
	if f == "" || strings.HasPrefix(f, "users/") {
		return f
	}
	return "users/me/dataSourceFamilies/" + f
}

// ListOptions controls ListDataPoints.
type ListOptions struct {
	DataType  string
	Start     Date // inclusive
	End       Date // exclusive
	Reconcile bool // use the reconciled stream when the type supports it
	Family    string
	Filter    string // raw AIP-160 filter; overrides Start/End
	PageToken string
	MaxPoints int // stop after this many points (default 200)
	MaxBytes  int // stop once the points reach roughly this size (default 120 KB)
}

// ListResult is what ListDataPoints returns.
type ListResult struct {
	DataType      string            `json:"dataType"`
	Method        string            `json:"method"`
	Filter        string            `json:"filter,omitempty"`
	Count         int               `json:"count"`
	DataPoints    []json.RawMessage `json:"dataPoints"`
	NextPageToken string            `json:"nextPageToken,omitempty"`
	Truncated     bool              `json:"truncated,omitempty"`
}

type listPage struct {
	DataPoints    []json.RawMessage `json:"dataPoints"`
	NextPageToken string            `json:"nextPageToken"`
}

func (c *Client) listPage(ctx context.Context, dt DataType, reconcile bool, filter, family, token string, pageSize int) (*listPage, error) {
	path := "/users/me/dataTypes/" + dt.ID + "/dataPoints"
	if reconcile {
		path += ":reconcile"
	}
	q := url.Values{}
	q.Set("pageSize", strconv.Itoa(pageSize))
	if filter != "" {
		q.Set("filter", filter)
	}
	if token != "" {
		q.Set("pageToken", token)
	}
	if f := familyName(family); f != "" {
		q.Set("dataSourceFamily", f)
	}
	raw, err := c.do(ctx, http.MethodGet, path, q, nil)
	if err != nil {
		return nil, err
	}
	var p listPage
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("decoding list response: %w", err)
	}
	return &p, nil
}

// ListDataPoints returns raw data points for a date range, newest first,
// following pagination up to MaxPoints / MaxBytes.
func (c *Client) ListDataPoints(ctx context.Context, o ListOptions) (*ListResult, error) {
	dt, err := Lookup(o.DataType)
	if err != nil {
		return nil, err
	}
	reconcile := o.Reconcile && dt.Reconcile
	if !dt.List && !reconcile {
		return nil, fmt.Errorf("%s can't be listed; use the daily totals or rollup tools instead", dt.ID)
	}
	if o.MaxPoints <= 0 {
		o.MaxPoints = 200
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 120_000
	}
	res := &ListResult{DataType: dt.ID, Method: "list", DataPoints: []json.RawMessage{}}
	if reconcile {
		res.Method = "reconcile"
	}

	candidates := []string{o.Filter}
	if o.Filter == "" {
		candidates = filterCandidates(dt, o.Start.String(), o.End.String())
	}

	pageSize := func() int { return min(o.MaxPoints-len(res.DataPoints), dt.MaxPageSize(), 1000) }

	// First page: try filter candidates until one is accepted.
	var page *listPage
	var firstErr error
	for i, f := range candidates {
		page, err = c.listPage(ctx, dt, reconcile, f, o.Family, o.PageToken, pageSize())
		if err == nil {
			res.Filter = f
			break
		}
		if i == 0 {
			firstErr = err
		}
		if !isInvalidArgument(err) || o.Filter != "" {
			return nil, err
		}
	}
	if page == nil {
		return nil, firstErr
	}

	size := 0
	for {
		dropped := false
		for _, p := range page.DataPoints {
			if len(res.DataPoints) >= o.MaxPoints || size >= o.MaxBytes {
				dropped = true
				break
			}
			res.DataPoints = append(res.DataPoints, p)
			size += len(p)
		}
		if dropped {
			// Part of this page was cut off, so its token would skip points;
			// the caller has to narrow the range instead of paging on.
			res.Truncated = true
			break
		}
		if page.NextPageToken == "" {
			break
		}
		if len(res.DataPoints) >= o.MaxPoints || size >= o.MaxBytes {
			res.Truncated = true
			res.NextPageToken = page.NextPageToken
			break
		}
		page, err = c.listPage(ctx, dt, reconcile, res.Filter, o.Family, page.NextPageToken, pageSize())
		if err != nil {
			return nil, err
		}
	}
	res.Count = len(res.DataPoints)
	return res, nil
}

// RollupResult is returned by DailyRollUp and RollUp.
type RollupResult struct {
	DataType  string            `json:"dataType"`
	Window    string            `json:"window"`
	Count     int               `json:"count"`
	Points    []json.RawMessage `json:"rollupDataPoints"`
	Truncated bool              `json:"truncated,omitempty"`
}

type rollupPage struct {
	Points        []json.RawMessage `json:"rollupDataPoints"`
	NextPageToken string            `json:"nextPageToken"`
}

// DailyRollUp aggregates a data type per civil day (or per windowDays
// days) over [start, end), splitting the range to respect API limits.
func (c *Client) DailyRollUp(ctx context.Context, dataType string, start, end Date, windowDays int, family string) (*RollupResult, error) {
	dt, err := Lookup(dataType)
	if err != nil {
		return nil, err
	}
	if !dt.DailyRollUp {
		return nil, fmt.Errorf("%s doesn't support daily rollups; list its data points instead", dt.ID)
	}
	if windowDays <= 0 {
		windowDays = 1
	}
	if !start.Before(end) {
		return nil, fmt.Errorf("start date %s must be before end date %s", start, end)
	}
	chunk := (dt.MaxRollupDays() / windowDays) * windowDays
	if chunk == 0 {
		return nil, fmt.Errorf("window_days %d exceeds the %d-day limit for %s", windowDays, dt.MaxRollupDays(), dt.ID)
	}
	res := &RollupResult{DataType: dt.ID, Window: fmt.Sprintf("%dd", windowDays), Points: []json.RawMessage{}}
	for cs := start; cs.Before(end); cs = cs.AddDays(chunk) {
		ce := cs.AddDays(chunk)
		if end.Before(ce) {
			ce = end
		}
		body := map[string]any{
			"range":          map[string]any{"start": cs.civilDateTime(), "end": ce.civilDateTime()},
			"windowSizeDays": windowDays,
			"pageSize":       min(dt.MaxRollupDays(), 90),
		}
		if f := familyName(family); f != "" {
			body["dataSourceFamily"] = f
		}
		for {
			raw, err := c.do(ctx, http.MethodPost, "/users/me/dataTypes/"+dt.ID+"/dataPoints:dailyRollUp", nil, body)
			if err != nil {
				return nil, err
			}
			var p rollupPage
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("decoding dailyRollUp response: %w", err)
			}
			res.Points = append(res.Points, p.Points...)
			if p.NextPageToken == "" {
				break
			}
			body["pageToken"] = p.NextPageToken
		}
	}
	res.Count = len(res.Points)
	return res, nil
}

// RollUp aggregates a data type into fixed windows over the physical time
// range [start, end), splitting the range to respect API limits.
func (c *Client) RollUp(ctx context.Context, dataType string, start, end time.Time, window time.Duration, family string, maxPoints int) (*RollupResult, error) {
	dt, err := Lookup(dataType)
	if err != nil {
		return nil, err
	}
	if !dt.RollUp {
		return nil, fmt.Errorf("%s doesn't support rollups; list its data points instead", dt.ID)
	}
	if window < time.Second {
		return nil, fmt.Errorf("window must be at least 1s")
	}
	if !start.Before(end) {
		return nil, fmt.Errorf("start %s must be before end %s", start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	if maxPoints <= 0 {
		maxPoints = 500
	}
	limit := time.Duration(dt.MaxRollupDays()) * 24 * time.Hour
	chunk := (limit / window) * window
	if chunk == 0 {
		return nil, fmt.Errorf("window %s exceeds the %d-day limit for %s", window, dt.MaxRollupDays(), dt.ID)
	}
	res := &RollupResult{DataType: dt.ID, Window: window.String(), Points: []json.RawMessage{}}
outer:
	for cs := start; cs.Before(end); cs = cs.Add(chunk) {
		ce := cs.Add(chunk)
		if ce.After(end) {
			ce = end
		}
		body := map[string]any{
			"range":      map[string]string{"startTime": cs.UTC().Format(time.RFC3339), "endTime": ce.UTC().Format(time.RFC3339)},
			"windowSize": durationString(window),
			"pageSize":   min(maxPoints, 10000),
		}
		if f := familyName(family); f != "" {
			body["dataSourceFamily"] = f
		}
		for {
			raw, err := c.do(ctx, http.MethodPost, "/users/me/dataTypes/"+dt.ID+"/dataPoints:rollUp", nil, body)
			if err != nil {
				return nil, err
			}
			var p rollupPage
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, fmt.Errorf("decoding rollUp response: %w", err)
			}
			for _, pt := range p.Points {
				if len(res.Points) >= maxPoints {
					res.Truncated = true
					break outer
				}
				res.Points = append(res.Points, pt)
			}
			if p.NextPageToken == "" {
				break
			}
			body["pageToken"] = p.NextPageToken
		}
	}
	res.Count = len(res.Points)
	return res, nil
}
