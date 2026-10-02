package mteam

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Search is read-only despite using POST. Retry only the failed page and
// promotion, keeping token generation and metainfo downloads out of this path.
func (c *Client) searchPage(ctx context.Context, body []byte) ([]byte, error) {
	const maxAttempts = 3
	client := c.credentialedHTTPClient()
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		request, err := c.request(ctx, http.MethodPost, "/api/torrent/search", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		retry := false
		if err == nil {
			var payload []byte
			payload, err = readResponse(response)
			if err == nil {
				return payload, nil
			}
			switch response.StatusCode {
			case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				retry = true
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var timeout net.Error
		if response == nil || response.StatusCode >= 200 && response.StatusCode < 300 {
			retry = errors.As(err, &timeout) && timeout.Timeout()
		}
		if !retry {
			return nil, err
		}
		if attempt == maxAttempts {
			return nil, fmt.Errorf("after %d attempts: %w", attempt, err)
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
