package remote

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestGoogleErrSummary(t *testing.T) {
	withCode := &oauth2.RetrieveError{
		Response:  &http.Response{StatusCode: 400},
		Body:      []byte("SECRET"),
		ErrorCode: "invalid_grant",
	}
	tests := []struct {
		name    string
		err     error
		want    []string
		notWant []string
	}{
		{"with error code", withCode, []string{"400", "invalid_grant"}, []string{"SECRET"}},
		{
			"without error code",
			&oauth2.RetrieveError{Response: &http.Response{StatusCode: 502}, Body: []byte("<html>SECRET")},
			[]string{"502"},
			[]string{"SECRET", "<html>"},
		},
		{"wrapped", fmt.Errorf("x: %w", withCode), []string{"400", "invalid_grant"}, []string{"SECRET"}},
		{
			"nil response",
			&oauth2.RetrieveError{ErrorCode: "invalid_grant", Body: []byte("SECRET")},
			[]string{"invalid_grant"},
			[]string{"SECRET"},
		},
		{"other type", errors.New("dial SECRET"), nil, []string{"SECRET"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := googleErrSummary(tt.err)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q does not contain %q", got, w)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("%q contains %q", got, w)
				}
			}
		})
	}
}
