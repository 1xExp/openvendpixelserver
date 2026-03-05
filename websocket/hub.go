package websocket

import (
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"
)

const DEBUG = true

func dbg(format string, args ...interface{}) {
	if DEBUG {
		log.Printf("[WS] "+format, args...)
	}
}

type WSMessage struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

type WSClient struct {
	Conn   *websocket.Conn
	Wallet string
	Send   chan WSMessage
	mu     sync.Mutex
}

type Hub struct {
	clients map[string]*WSClient
	mu      sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{clients: make(map[string]*WSClient)}
}

func (h *Hub) Register(wallet string, client *WSClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.clients[wallet]; ok {
		old.Conn.Close()
		close(old.Send)
		dbg("replaced existing connection: %s", wallet)
	}
	h.clients[wallet] = client
}

func (h *Hub) Remove(wallet string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.clients[wallet]; ok {
		close(c.Send)
		delete(h.clients, wallet)
	}
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) Broadcast(msg WSMessage, excludeWallet string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for wallet, c := range h.clients {
		if wallet == excludeWallet {
			continue
		}
		select {
		case c.Send <- msg:
		default:
			dbg("send buffer full, dropped msg for %s", wallet)
		}
	}
}

func (h *Hub) Send(wallet string, msg WSMessage) {
	h.mu.RLock()
	c, ok := h.clients[wallet]
	h.mu.RUnlock()
	if ok {
		select {
		case c.Send <- msg:
		default:
			dbg("send buffer full, dropped msg for %s", wallet)
		}
	}
}

func (h *Hub) SnapshotPlayers(excludeWallet string, fetchPlayer func(wallet string) (map[string]interface{}, error)) []map[string]interface{} {
	h.mu.RLock()
	wallets := make([]string, 0, len(h.clients))
	for w := range h.clients {
		if w != excludeWallet {
			wallets = append(wallets, w)
		}
	}
	h.mu.RUnlock()

	players := make([]map[string]interface{}, 0, len(wallets))
	for _, w := range wallets {
		data, err := fetchPlayer(w)
		if err != nil {
			dbg("snapshot: failed to fetch player %s: %v", w, err)
			continue
		}
		players = append(players, data)
	}
	return players
}

func CleanupOfflinePlayers(interval time.Duration, cleanup func()) {
	go func() {
		for {
			time.Sleep(interval)
			cleanup()
		}
	}()
}

func ClientWriter(client *WSClient) {
	for msg := range client.Send {
		if err := client.Conn.WriteJSON(msg); err != nil {
			dbg("write error for %s: %v", client.Wallet, err)
			break
		}
	}
	client.Conn.Close()
}

func ClientReader(
	client *WSClient,
	hub *Hub,
	getPosition func(wallet string) (float64, float64, error),
	savePosition func(wallet string, x, y float64),
	getDisplayName func(wallet string) (string, error),
	onDisconnect func(wallet string),
	onHeartbeat func(wallet string),
) {
	defer func() {
		hub.Remove(client.Wallet)
		client.Conn.Close()
		onDisconnect(client.Wallet)
		hub.Broadcast(WSMessage{
			Type: "player_leave",
			Data: map[string]interface{}{"wallet": client.Wallet},
		}, client.Wallet)
		dbg("disconnected: %s (remaining: %d)", client.Wallet, hub.Count())
	}()

	for {
		var msg WSMessage
		if err := client.Conn.ReadJSON(&msg); err != nil {
			dbg("read error for %s: %v", client.Wallet, err)
			break
		}

		switch msg.Type {
		case "position":
			x, okX := msg.Data["x"].(float64)
			y, okY := msg.Data["y"].(float64)
			if !okX || !okY {
				continue
			}

			curX, curY, err := getPosition(client.Wallet)
			if err != nil {
				continue
			}
			dx := x - curX
			dy := y - curY
			if dx*dx+dy*dy > 600*600 {
				dbg("anti-teleport blocked: %s (%.0f,%.0f)", client.Wallet, x, y)
				continue
			}

			savePosition(client.Wallet, x, y)
			hub.Broadcast(WSMessage{
				Type: "player_move",
				Data: map[string]interface{}{
					"wallet": client.Wallet,
					"x":      x,
					"y":      y,
				},
			}, client.Wallet)

		case "chat":
			message, ok := msg.Data["message"].(string)
			if !ok || message == "" {
				continue
			}
			message = sanitizeMessage(message)
			if len(message) == 0 {
				continue
			}

			senderName, err := getDisplayName(client.Wallet)
			if err != nil {
				senderName = client.Wallet[:6] + "..."
			}

			hub.Broadcast(WSMessage{
				Type: "chat",
				Data: map[string]interface{}{
					"wallet":  client.Wallet,
					"sender":  senderName,
					"message": message,
					"time":    time.Now().Unix(),
				},
			}, "")
			dbg("chat [%s]: %s", senderName, message)

		case "heartbeat":
			onHeartbeat(client.Wallet)
		}
	}
}

func sanitizeMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	msg = strings.ReplaceAll(msg, "\n", " ")
	msg = strings.ReplaceAll(msg, "\r", " ")
	msg = regexp.MustCompile(`<[^>]*>`).ReplaceAllString(msg, "")
	return msg
}