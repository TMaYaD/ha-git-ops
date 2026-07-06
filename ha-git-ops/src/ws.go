package main

// Minimal Home Assistant websocket client via the Supervisor core proxy.
// Only what Tier 2 needs: authenticated one-shot commands.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// LovelaceSave saves a dashboard config. Empty urlPath targets the
// default dashboard.
func (h *HA) LovelaceSave(urlPath string, config any) error {
	cmd := map[string]any{"type": "lovelace/config/save", "config": config}
	if urlPath != "" {
		cmd["url_path"] = urlPath
	}
	return h.wsCall(cmd)
}

func (h *HA) wsCall(cmd map[string]any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, "ws://supervisor/core/websocket", nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(8 << 20)

	var msg map[string]any
	if err := wsjson.Read(ctx, conn, &msg); err != nil { // auth_required
		return fmt.Errorf("ws handshake: %w", err)
	}
	if err := wsjson.Write(ctx, conn, map[string]any{
		"type": "auth", "access_token": h.token}); err != nil {
		return err
	}
	if err := wsjson.Read(ctx, conn, &msg); err != nil {
		return err
	}
	if msg["type"] != "auth_ok" {
		return fmt.Errorf("ws auth failed: %v", msg["type"])
	}

	cmd["id"] = 1
	if err := wsjson.Write(ctx, conn, cmd); err != nil {
		return err
	}
	for {
		var resp struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Success bool            `json:"success"`
			Error   json.RawMessage `json:"error"`
		}
		if err := wsjson.Read(ctx, conn, &resp); err != nil {
			return err
		}
		if resp.Type == "result" && resp.ID == 1 {
			if !resp.Success {
				return fmt.Errorf("%s: %s", cmd["type"], resp.Error)
			}
			return nil
		}
	}
}
