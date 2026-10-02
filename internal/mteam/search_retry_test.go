package mteam

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const searchResult = `{"code":0,"data":{"data":[]}}`

const oneSearchResult = `{"code":0,"data":{"data":[{"id":1,"name":"candidate","size":1,"createdDate":"2099-01-01 00:00:00","status":{"discount":"FREE","discountEndTime":null,"seeders":1,"leechers":1}}]}}`

type searchTimeout struct{}

func (searchTimeout) Error() string   { return "search request timed out" }
func (searchTimeout) Timeout() bool   { return true }
func (searchTimeout) Temporary() bool { return true }

type closingBody struct {
	io.Reader
	closed bool
}

func (b *closingBody) Close() error {
	b.closed = true
	return nil
}

func searchResponse(request *http.Request, status int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     make(http.Header),
		Body:       body,
		Request:    request,
	}
}

func TestSearchRetriesTransientPageAndReplaysRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var freeCalls, doubleCalls int
		var replayed string
		transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if request.Method != http.MethodPost || request.URL.Path != "/api/torrent/search" || request.Header.Get("Content-Type") != "application/json" || request.Header.Get("x-api-key") != "key" {
				t.Fatalf("unexpected request: %s %s headers=%v", request.Method, request.URL, request.Header)
			}
			if strings.Contains(string(body), `"discount":"FREE"`) {
				freeCalls++
				return searchResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(oneSearchResult))), nil
			}
			if !strings.Contains(string(body), `"discount":"_2X_FREE"`) {
				t.Fatalf("unexpected search body %s", body)
			}
			doubleCalls++
			if doubleCalls == 1 {
				replayed = string(body)
				return searchResponse(request, http.StatusBadGateway, io.NopCloser(strings.NewReader("upstream unavailable"))), nil
			}
			if string(body) != replayed {
				t.Fatalf("retry body=%s, want %s", body, replayed)
			}
			return searchResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(searchResult))), nil
		})

		got, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(context.Background())
		if err != nil || len(got) != 1 || freeCalls != 1 || doubleCalls != 2 {
			t.Fatalf("Search results=%v error=%v FREE calls=%d _2X_FREE calls=%d", got, err, freeCalls, doubleCalls)
		}
	})
}

func TestSearchRetriesBodyReadTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		failedBody := &closingBody{Reader: failingReader{err: searchTimeout{}}}
		transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return searchResponse(request, http.StatusOK, failedBody), nil
			}
			return searchResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(searchResult))), nil
		})

		_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(context.Background())
		if err != nil || calls != 3 || !failedBody.closed {
			t.Fatalf("Search error=%v calls=%d failed body closed=%t", err, calls, failedBody.closed)
		}
	})
}

func TestSearchRetriesHeaderTimeoutAndGatewayTimeout(t *testing.T) {
	for name, first := range map[string]func(*http.Request) (*http.Response, error){
		"header timeout": func(*http.Request) (*http.Response, error) {
			return nil, searchTimeout{}
		},
		"gateway timeout": func(request *http.Request) (*http.Response, error) {
			return searchResponse(request, http.StatusGatewayTimeout, io.NopCloser(strings.NewReader("upstream timeout"))), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						return first(request)
					}
					return searchResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(searchResult))), nil
				})
				_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(context.Background())
				if err != nil || calls != 3 {
					t.Fatalf("Search error=%v calls=%d", err, calls)
				}
			})
		})
	}
}

func TestSearchStopsAfterTransientRetryExhaustionWithoutPartialResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var freeCalls, doubleCalls int
		var bodies []*closingBody
		started := time.Now()
		transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), `"discount":"FREE"`) {
				freeCalls++
				return searchResponse(request, http.StatusOK, io.NopCloser(strings.NewReader(oneSearchResult))), nil
			}
			doubleCalls++
			responseBody := &closingBody{Reader: strings.NewReader("upstream unavailable")}
			bodies = append(bodies, responseBody)
			return searchResponse(request, http.StatusServiceUnavailable, responseBody), nil
		})

		got, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(context.Background())
		if err == nil || got != nil || freeCalls != 1 || doubleCalls != 3 {
			t.Fatalf("Search results=%v error=%v FREE calls=%d _2X_FREE calls=%d", got, err, freeCalls, doubleCalls)
		}
		if !strings.Contains(err.Error(), "search page 1 discount _2X_FREE: after 3 attempts:") || time.Since(started) != 3*time.Second {
			t.Fatalf("Search error=%v elapsed=%s", err, time.Since(started))
		}
		for i, body := range bodies {
			if !body.closed {
				t.Errorf("response body %d was not closed", i)
			}
		}
	})
}

func TestSearchPreservesTimeoutAfterRetryExhaustion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		transport := roundTripperFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, context.DeadlineExceeded
		})
		_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(context.Background())
		if !errors.Is(err, context.DeadlineExceeded) || calls != 3 {
			t.Fatalf("Search error=%v calls=%d", err, calls)
		}
	})
}

func TestSearchDoesNotRetryCanceledOrExpiredContext(t *testing.T) {
	t.Run("canceled after retryable response", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				cancel()
				return searchResponse(request, http.StatusBadGateway, io.NopCloser(strings.NewReader("upstream unavailable"))), nil
			})

			_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(ctx)
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("Search error=%v calls=%d", err, calls)
			}
		})
	})
	t.Run("expired before request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now())
			defer cancel()
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return searchResponse(request, http.StatusBadGateway, io.NopCloser(strings.NewReader("upstream unavailable"))), nil
			})

			_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(ctx)
			if !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
				t.Fatalf("Search error=%v calls=%d", err, calls)
			}
		})
	})
	t.Run("deadline during retry delay", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return searchResponse(request, http.StatusBadGateway, io.NopCloser(strings.NewReader("upstream unavailable"))), nil
			})
			done := make(chan error, 1)
			go func() {
				_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(ctx)
				done <- err
			}()
			time.Sleep(time.Second)
			err := <-done
			if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
				t.Fatalf("Search error=%v calls=%d", err, calls)
			}
		})
	})
}

func TestSearchDoesNotRetryTerminalResponses(t *testing.T) {
	for name, response := range map[string]struct {
		status int
		body   string
	}{
		"bad request":    {status: http.StatusBadRequest, body: "terminal failure"},
		"server error":   {status: http.StatusInternalServerError, body: "terminal failure"},
		"API error":      {status: http.StatusOK, body: `{"code":42,"message":"denied","data":null}`},
		"malformed JSON": {status: http.StatusOK, body: `{`},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return searchResponse(request, response.status, io.NopCloser(strings.NewReader(response.body))), nil
			})
			_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(context.Background())
			if err == nil || calls != 1 {
				t.Fatalf("Search error=%v calls=%d", err, calls)
			}
		})
	}
}

func TestSearchDoesNotRetryTerminalHTTPBodyTimeout(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			body := &closingBody{Reader: failingReader{err: searchTimeout{}}}
			transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return searchResponse(request, status, body), nil
			})

			_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Search(context.Background())
			if err == nil || calls != 1 || !body.closed {
				t.Fatalf("Search error=%v calls=%d body closed=%t", err, calls, body.closed)
			}
		})
	}
}

func TestDownloadDoesNotRetryTransientFailures(t *testing.T) {
	calls := 0
	transport := roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return searchResponse(request, http.StatusBadGateway, io.NopCloser(strings.NewReader("upstream unavailable"))), nil
	})
	_, err := testClient(t, "https://mteam.test", Config{HTTPClient: &http.Client{Transport: transport}}).Download(context.Background(), 1)
	if err == nil || calls != 1 {
		t.Fatalf("Download error=%v calls=%d", err, calls)
	}
}

type failingReader struct {
	err error
}

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }
