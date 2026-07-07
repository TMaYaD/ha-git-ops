package main

// Supervisor / Core API client. Runs with the add-on's SUPERVISOR_TOKEN.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const supervisor = "http://supervisor"

type HA struct {
	token  string
	client *http.Client

	// NotifyService is an optional "domain.service" (e.g.
	// "notify.mobile_app_phone") that Notify additionally pushes
	// through; empty means persistent notifications only.
	NotifyService string
}

func NewHA() *HA {
	return &HA{
		token:  containerEnv("SUPERVISOR_TOKEN"),
		client: &http.Client{Timeout: 90 * time.Second},
	}
}

// containerEnv reads a container environment variable. s6-overlay v3 (in
// the HA base images) strips the environment for the CMD process and
// exposes the container env as files instead.
func containerEnv(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	b, err := os.ReadFile("/run/s6/container_environment/" + name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h *HA) req(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, supervisor+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, err
}

// CoreCheck runs `ha core check`; false means the config is invalid.
func (h *HA) CoreCheck() (bool, error) {
	status, raw, err := h.req("POST", "/core/check", nil)
	if err != nil {
		return false, fmt.Errorf("core check request: %w", err)
	}
	var body struct {
		Result string `json:"result"`
	}
	_ = json.Unmarshal(raw, &body)
	ok := status == 200 && body.Result == "ok"
	if !ok {
		log.Printf("core config check failed: %s", raw)
	}
	return ok, nil
}

func (h *HA) CoreRestart() {
	if _, raw, err := h.req("POST", "/core/restart", nil); err != nil {
		log.Printf("core restart failed: %v %s", err, raw)
	}
}

func (h *HA) CallService(domain, service string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	status, raw, err := h.req("POST",
		"/core/api/services/"+domain+"/"+service, data)
	if err != nil || status >= 400 {
		log.Printf("service %s.%s failed: %v %s", domain, service, err, raw)
	}
}

func (h *HA) SetState(entityID, state string, attributes map[string]any) {
	status, raw, err := h.req("POST", "/core/api/states/"+entityID,
		map[string]any{"state": state, "attributes": attributes})
	if err != nil || status >= 400 {
		log.Printf("set state %s failed: %v %s", entityID, err, raw)
	}
}

// Persist creates or replaces the persistent notification keyed by title.
func (h *HA) Persist(title, message string) {
	h.CallService("persistent_notification", "create", map[string]any{
		"title":           title,
		"message":         message,
		"notification_id": "ha_gitops_" + sanitizeID(title),
	})
}

// Dismiss removes the persistent notification keyed by title.
func (h *HA) Dismiss(title string) {
	h.CallService("persistent_notification", "dismiss", map[string]any{
		"notification_id": "ha_gitops_" + sanitizeID(title),
	})
}

// Notify persists in the UI and, when a notify service is configured,
// pushes through it as well so alerts reach a phone.
func (h *HA) Notify(title, message string) {
	h.Persist(title, message)
	if h.NotifyService == "" {
		return
	}
	domain, service, ok := strings.Cut(h.NotifyService, ".")
	if !ok {
		log.Printf("notify_service %q is not domain.service; skipping push", h.NotifyService)
		return
	}
	h.CallService(domain, service, map[string]any{"title": title, "message": message})
}

func sanitizeID(s string) string {
	out := make([]rune, 0, len(s))
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
