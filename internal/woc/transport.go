package woc

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	bitfsdebug "github.com/bsv8/BitFSProtocol/debug"
)

// do paces this client's requests and retries only HTTP throttling. POST retries
// use Request.GetBody: identical bytes, never a newly built or signed transaction.
func (c *Client) do(request *http.Request) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		c.requestMu.Lock()
		sendAt := c.nextRequest
		if sendAt.Before(time.Now()) {
			sendAt = time.Now()
		}
		c.nextRequest = sendAt.Add(350 * time.Millisecond)
		c.requestMu.Unlock()
		if err := waitRequest(request, time.Until(sendAt)); err != nil {
			return nil, err
		}
		started := time.Now()
		bitfsdebug.Log("keymaster.cc-cli", "chain.http.start", func() any {
			return map[string]any{"method": request.Method, "host": request.URL.Host, "path": request.URL.Path, "attempt": attempt + 1}
		})
		response, err := c.http.Do(request)
		bitfsdebug.Log("keymaster.cc-cli", "chain.http.finish", func() any {
			status := 0
			if response != nil {
				status = response.StatusCode
			}
			return map[string]any{"method": request.Method, "path": request.URL.Path, "attempt": attempt + 1, "httpStatus": status, "elapsedMs": float64(time.Since(started).Microseconds()) / 1000, "error": err}
		})
		if err != nil || response.StatusCode != http.StatusTooManyRequests || attempt >= 3 {
			return response, err
		}
		if request.Body != nil && request.GetBody == nil {
			return response, nil
		}
		delay := time.Second << attempt
		if value := response.Header.Get("Retry-After"); value != "" {
			if seconds, err := strconv.ParseUint(value, 10, 32); err == nil {
				delay = time.Duration(seconds) * time.Second
			} else if at, err := http.ParseTime(value); err == nil {
				delay = time.Until(at)
			}
		}
		// If the provider asks for a longer pause, retain its exact unknown receipt
		// instead of retrying earlier than requested or blocking a session indefinitely.
		if delay > 30*time.Second {
			return response, nil
		}
		if delay < time.Second {
			delay = time.Second
		}
		response.Body.Close()
		bitfsdebug.Log("keymaster.cc-cli", "chain.http.backoff", func() any {
			return map[string]any{"path": request.URL.Path, "attempt": attempt + 1, "delayMs": delay.Milliseconds(), "httpStatus": 429}
		})
		if err := waitRequest(request, delay); err != nil {
			return nil, err
		}
		next := request.Clone(request.Context())
		if request.GetBody != nil {
			next.Body, err = request.GetBody()
			if err != nil {
				return nil, fmt.Errorf("woc: replay exact request: %w", err)
			}
		}
		request = next
	}
}
func waitRequest(request *http.Request, delay time.Duration) error {
	if err := request.Context().Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-request.Context().Done():
		return request.Context().Err()
	case <-timer.C:
		return nil
	}
}
