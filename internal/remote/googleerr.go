package remote

import (
	"errors"
	"fmt"

	"golang.org/x/oauth2"
)

// RetrieveError.Error() embeds the response body when Google returns no
// error code, so it must not reach the logs.
func googleErrSummary(err error) string {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) {
		return fmt.Sprintf("%T", err)
	}
	status := 0
	if re.Response != nil {
		status = re.Response.StatusCode
	}
	return fmt.Sprintf("status=%d error_code=%q", status, re.ErrorCode)
}
