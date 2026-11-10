package channel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func panelHTTPError(err error, elapsed time.Duration, status int) string {
	var transport *url.Error
	if errors.As(err, &transport) {
		err = transport.Err // The wrapper includes the request URL, which may contain credentials.
	}
	var network net.Error
	timeout := errors.As(err, &network) && network.Timeout()
	var operation *net.OpError
	if errors.As(err, &operation) {
		err = operation.Err // Connection addresses are unnecessary for classification.
	}
	return fmt.Sprintf("status=%d elapsed=%s error_type=%T timeout=%t deadline=%t message=%q", status, elapsed, err, timeout, errors.Is(err, context.DeadlineExceeded), err.Error())
}

func TestPanelHTTPErrorOmitsRequestURL(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "http://user:synthetic-password@localhost/login?csrf=synthetic-csrf", Err: context.DeadlineExceeded}
	got := panelHTTPError(err, time.Second, 0)
	for _, secret := range []string{"synthetic-password", "synthetic-csrf", "http://", "user:"} {
		if strings.Contains(got, secret) {
			t.Fatal("transport diagnostic exposed request URL")
		}
	}
	for _, want := range []string{"status=0", "elapsed=1s", "error_type=context.deadlineExceededError", "timeout=true", "deadline=true", "context deadline exceeded"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing diagnostic %q", want)
		}
	}
}
