package controller

import (
	"bytes"
	"encoding/json"
	"net/http"

	"sdwan/internal/model"
)

// HTTP routes exposed by Server.
const (
	RoutePolicy   = "/policy"
	RoutePull     = "/policy/pull?edge="
	RouteReport   = "/telemetry"
	RouteSnapshot = "/state"
)

// RegisterHTTP mounts the controller onto a mux for real-device deployments.
func (c *Controller) RegisterHTTP(mux *http.ServeMux) {
	mux.HandleFunc("/policy", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, c.CurrentPolicy())
		case http.MethodPut, http.MethodPost:
			var p model.Policy
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, c.PushPolicy(&p))
		}
	})
	mux.HandleFunc("/policy/pull", func(w http.ResponseWriter, r *http.Request) {
		edge := r.URL.Query().Get("edge")
		p, err := c.PullPolicy(edge)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, p)
	})
	mux.HandleFunc("/telemetry", func(w http.ResponseWriter, r *http.Request) {
		var t model.EdgeTelemetry
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := c.Report(t); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, c.Snapshot())
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// HTTPClient is the Client implementation used by real edge devices.
type HTTPClient struct {
	BaseURL  string
	EdgeID   string
	http     *http.Client
	fallback *model.Policy
}

// NewHTTPClient builds a client with a local fallback policy for offline boot.
func NewHTTPClient(baseURL, edgeID string, fallback *model.Policy) *HTTPClient {
	return &HTTPClient{BaseURL: baseURL, EdgeID: edgeID, http: http.DefaultClient, fallback: fallback}
}

// Pull fetches policy over HTTP; on any transport failure it returns the last
// good local policy together with the error, so the edge keeps forwarding.
func (h *HTTPClient) Pull(edgeID string) (*model.Policy, error) {
	resp, err := h.http.Get(h.BaseURL + "/policy/pull?edge=" + edgeID)
	if err != nil {
		return h.fallback, ErrUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h.fallback, ErrUnreachable
	}
	var p model.Policy
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return h.fallback, err
	}
	h.fallback = &p
	return &p, nil
}

// Report posts telemetry over HTTP.
func (h *HTTPClient) Report(t model.EdgeTelemetry) error {
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	resp, err := h.http.Post(h.BaseURL+"/telemetry", "application/json", bytes.NewReader(body))
	if err != nil {
		return ErrUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return ErrUnreachable
	}
	return nil
}
