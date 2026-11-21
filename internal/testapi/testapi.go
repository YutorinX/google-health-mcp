// Package testapi is a fake Google Health API for tests.
package testapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Request is one recorded call.
type Request struct {
	Method string
	Path   string
	Query  map[string]string
	Body   map[string]any
}

// Server records requests and answers with Handler.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	Requests []Request
	// Handler returns status and JSON body for a request.
	Handler func(r Request) (int, string)
}

func New(t *testing.T, h func(r Request) (int, string)) *Server {
	s := &Server{Handler: h}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, hr *http.Request) {
		r := Request{Method: hr.Method, Path: hr.URL.Path, Query: map[string]string{}}
		for k, v := range hr.URL.Query() {
			r.Query[k] = v[0]
		}
		if b, _ := io.ReadAll(hr.Body); len(b) > 0 {
			if err := json.Unmarshal(b, &r.Body); err != nil {
				t.Errorf("bad JSON body: %v", err)
			}
		}
		s.mu.Lock()
		s.Requests = append(s.Requests, r)
		s.mu.Unlock()
		code, body := s.Handler(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Calls() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.Requests...)
}

// Error builds a Google-style error body.
func Error(code int, status, msg, reason string) string {
	details := "[]"
	if reason != "" {
		details = fmt.Sprintf(`[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":%q,"domain":"health.googleapis.com"}]`, reason)
	}
	return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"status":%q,"details":%s}}`, code, msg, status, details)
}

// Points builds a list response with n points of the given type.
func Points(field string, n int, next string) string {
	var pts []string
	for i := range n {
		pts = append(pts, fmt.Sprintf(`{"name":"users/me/dataTypes/x/dataPoints/%d","dataSource":{"platform":"FITBIT"},%q:{"value":%d}}`, i, field, i))
	}
	return fmt.Sprintf(`{"dataPoints":[%s],"nextPageToken":%q}`, strings.Join(pts, ","), next)
}
