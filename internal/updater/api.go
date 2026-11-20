package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The HTTP API, served on the compose network only:
//
//	GET  /status    Status
//	POST /update    202 and a Result; 200 and a Result if already up to date
//	POST /rollback  202 and a Result
//
// Errors are {"error": "..."}, with 409 when busy.

// Handler serves u's API.
func (u *Updater) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		s, err := u.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	for _, action := range []string{"update", "rollback"} {
		mux.HandleFunc("POST /"+action, func(w http.ResponseWriter, r *http.Request) {
			res, err := u.Start(r.Context(), action)
			switch {
			case errors.Is(err, ErrUpToDate):
				writeJSON(w, http.StatusOK, res)
			case errors.Is(err, ErrBusy):
				writeError(w, http.StatusConflict, err)
			case err != nil:
				writeError(w, http.StatusInternalServerError, err)
			default:
				writeJSON(w, http.StatusAccepted, res)
			}
		})
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Client talks to an updater from steel.
type Client struct {
	URL  string // e.g. http://updater:8086
	HTTP *http.Client
}

// Status asks for the updater's Status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	_, err := c.call(ctx, http.MethodGet, "/status", &s)
	return s, err
}

// Start asks for an update or rollback (action "update" or "rollback").
// The updater replies before doing the work, which ends with steel's
// container replaced: Status reports how it went. ErrUpToDate and ErrBusy
// come back as themselves.
func (c *Client) Start(ctx context.Context, action string) (Result, error) {
	var r Result
	status, err := c.call(ctx, http.MethodPost, "/"+action, &r)
	if err == nil && status == http.StatusOK {
		err = ErrUpToDate
	}
	return r, err
}

func (c *Client) call(ctx context.Context, method, path string, out any) (int, error) {
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reaching the updater: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) != nil || e.Error == "" {
			e.Error = resp.Status
		}
		if resp.StatusCode == http.StatusConflict {
			return resp.StatusCode, ErrBusy
		}
		return resp.StatusCode, fmt.Errorf("updater: %s", e.Error)
	}
	return resp.StatusCode, json.Unmarshal(b, out)
}
