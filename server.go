package main

import (
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/golang-jwt/jwt/v5"
	"github.com/joho/godotenv"
	gormPostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// ─────────────────────────── Models ────────────────────────────

type Player struct {
	ID            uint      `gorm:"primarykey" json:"id"`
	WalletAddress string    `gorm:"unique;not null" json:"wallet_address"`
	Username      *string   `json:"username"`
	Level         int       `gorm:"default:1" json:"level"`
	Experience    int       `gorm:"default:0" json:"experience"`
	Gold          int       `gorm:"default:0" json:"gold"`
	PositionX     float64   `gorm:"default:100" json:"position_x"`
	PositionY     float64   `gorm:"default:100" json:"position_y"`
	IsOnline      bool      `gorm:"default:false" json:"is_online"`
	LastSeen      time.Time `json:"last_seen"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Claims struct {
	WalletAddress string `json:"wallet_address"`
	jwt.RegisteredClaims
}

// ─────────────────────────── WebSocket ─────────────────────────

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

// ─────────────────────────── Hub ───────────────────────────────

type Hub struct {
	clients map[string]*WSClient
	mu      sync.RWMutex
}

func newHub() *Hub {
	return &Hub{clients: make(map[string]*WSClient)}
}

func (h *Hub) remove(wallet string) {
	h.mu.Lock()
	if c, ok := h.clients[wallet]; ok {
		close(c.Send)
		delete(h.clients, wallet)
	}
	h.mu.Unlock()
}

func (h *Hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// broadcast sends msg to all clients except excludeWallet.
func (h *Hub) broadcast(msg WSMessage, excludeWallet string) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for wallet, c := range h.clients {
		if wallet == excludeWallet {
			continue
		}
		select {
		case c.Send <- msg:
		default:
			// drop if buffer full
		}
	}
}

// send sends msg to a specific client.
func (h *Hub) send(wallet string, msg WSMessage) {
	h.mu.RLock()
	c, ok := h.clients[wallet]
	h.mu.RUnlock()
	if ok {
		select {
		case c.Send <- msg:
		default:
		}
	}
}

// snapshotPlayers returns a list of all online players except excludeWallet.
func (h *Hub) snapshotPlayers(excludeWallet string) []map[string]interface{} {
	h.mu.RLock()
	wallets := make([]string, 0, len(h.clients))
	for w := range h.clients {
		if w != excludeWallet {
			wallets = append(wallets, w)
		}
	}
	h.mu.RUnlock()

	var players []map[string]interface{}
	for _, w := range wallets {
		var p Player
		if err := db.Where("wallet_address = ?", w).First(&p).Error; err != nil {
			continue
		}

		// Pakai wallet address kalau username kosong
		username := w[:6] + "..."
		if p.Username != nil {
			username = *p.Username
		}

		players = append(players, map[string]interface{}{
			"wallet":   w,
			"x":        p.PositionX,
			"y":        p.PositionY,
			"username": username,
		})
	}
	return players
}

// ─────────────────────────── Globals ───────────────────────────

var (
	db        *gorm.DB
	jwtSecret string
	hub       = newHub()
)

// ─────────────────────────── main ──────────────────────────────

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("⚠️  No .env file found")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	jwtSecret = os.Getenv("JWT_SECRET")
	if databaseURL == "" || jwtSecret == "" {
		log.Fatal("❌ Missing DATABASE_URL or JWT_SECRET")
	}

	var err error
	db, err = gorm.Open(gormPostgres.Open(databaseURL), &gorm.Config{PrepareStmt: false})
	if err != nil {
		log.Fatal("❌ Failed to connect to DB:", err)
	}
	db.AutoMigrate(&Player{})
	fmt.Println("✅ Connected to PostgreSQL")

	// Mark all players offline at startup
	db.Model(&Player{}).Where("is_online = ?", true).Update("is_online", false)

	go cleanupOfflinePlayers()

	// ── Fiber ─────────────────────────────────────────────────
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Origin, Content-Type, Accept, Authorization",
	}))

	// REST routes
	app.Post("/auth/wallet", handleWalletAuth)
	app.Get("/player/me", authMiddleware, getPlayerProfile)
	app.Put("/player/me", authMiddleware, updatePlayerProfile)
	app.Post("/player/position", authMiddleware, updatePlayerPosition)
	app.Get("/server/info", getServerInfo)
	app.Post("/server/join", authMiddleware, joinServer)
	app.Post("/player/heartbeat", authMiddleware, playerHeartbeat)
	app.Post("/player/disconnect", authMiddleware, playerDisconnect)
	app.Get("/players/online", authMiddleware, getOnlinePlayers)

	// WebSocket upgrade middleware — validasi token sebelum upgrade
	app.Use("/ws", func(c *fiber.Ctx) error {
		token := c.Query("token")
		if token == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing token"})
		}
		wallet, err := verifyJWT(token)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}
		// Simpan wallet ke locals supaya bisa diambil di WS handler
		c.Locals("wallet", wallet)

		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	// WebSocket handler — satu port dengan REST
	app.Get("/ws", websocket.New(handleFiberWebSocket))

	// Railway / production: baca PORT dari env, fallback ke 8080
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("🚀 Server listening on :%s  (REST + WebSocket ws://...:%s/ws)\n", port, port)
	log.Fatal(app.Listen(":" + port))
}

// ─────────────────────────── WebSocket handler ─────────────────

func handleFiberWebSocket(c *websocket.Conn) {
	wallet := c.Locals("wallet").(string)

	client := &WSClient{
		Conn:   c,
		Wallet: wallet,
		Send:   make(chan WSMessage, 256),
	}

	// Kalau wallet yang sama reconnect, tutup koneksi lama dulu.
	hub.mu.Lock()
	if old, ok := hub.clients[wallet]; ok {
		old.Conn.Close()
		close(old.Send)
	}
	hub.clients[wallet] = client
	hub.mu.Unlock()

	fmt.Printf("✅ WS connected: %s  (total: %d)\n", wallet, hub.count())

	// ① Kirim daftar player yang sudah online ke newcomer.
	go func() {
		players := hub.snapshotPlayers(wallet)
		hub.send(wallet, WSMessage{
			Type: "init_players",
			Data: map[string]interface{}{"players": players},
		})
		fmt.Printf("📤 init_players → %s  (%d players)\n", wallet, len(players))
	}()

	// ② Broadcast player_join ke semua yang lain.
	go func() {
		var p Player
		db.Where("wallet_address = ?", wallet).First(&p)

		username := wallet[:6] + "..."
		if p.Username != nil {
			username = *p.Username
		}

		hub.broadcast(WSMessage{
			Type: "player_join",
			Data: map[string]interface{}{
				"wallet":   wallet,
				"x":        p.PositionX,
				"y":        p.PositionY,
				"username": username,
			},
		}, wallet)
		fmt.Printf("📢 player_join broadcast for %s\n", wallet)
	}()

	go clientWriter(client)
	clientReader(client) // blocks sampai disconnect
}

// clientReader membaca pesan dari player; cleanup saat koneksi putus.
func clientReader(client *WSClient) {
	defer func() {
		hub.remove(client.Wallet)
		client.Conn.Close()

		// Tandai offline di DB.
		db.Model(&Player{}).Where("wallet_address = ?", client.Wallet).Updates(map[string]interface{}{
			"is_online": false,
			"last_seen": time.Now(),
		})

		// Broadcast player_leave ke semua yang tersisa.
		hub.broadcast(WSMessage{
			Type: "player_leave",
			Data: map[string]interface{}{"wallet": client.Wallet},
		}, client.Wallet)

		fmt.Printf("❌ WS disconnected: %s  (remaining: %d)\n", client.Wallet, hub.count())
	}()

	for {
		var msg WSMessage
		if err := client.Conn.ReadJSON(&msg); err != nil {
			break
		}

		switch msg.Type {

		case "position":
			x, okX := msg.Data["x"].(float64)
			y, okY := msg.Data["y"].(float64)
			if !okX || !okY {
				continue
			}

			// Anti-teleport: max 600 px per tick.
			var cur Player
			db.Select("position_x, position_y").Where("wallet_address = ?", client.Wallet).First(&cur)
			dx := x - cur.PositionX
			dy := y - cur.PositionY
			dist := dx*dx + dy*dy
			const maxDist = 600 * 600
			if dist > maxDist {
				continue
			}

			// Simpan posisi ke DB.
			db.Model(&Player{}).Where("wallet_address = ?", client.Wallet).Updates(map[string]interface{}{
				"position_x": x,
				"position_y": y,
				"last_seen":  time.Now(),
			})

			// Broadcast posisi ke player lain.
			hub.broadcast(WSMessage{
				Type: "player_move",
				Data: map[string]interface{}{
					"wallet": client.Wallet,
					"x":      x,
					"y":      y,
				},
			}, client.Wallet)

		case "heartbeat":
			db.Model(&Player{}).Where("wallet_address = ?", client.Wallet).Update("last_seen", time.Now())
		}
	}
}

// clientWriter mengosongkan send channel dan menulis ke WebSocket.
func clientWriter(client *WSClient) {
	for msg := range client.Send {
		if err := client.Conn.WriteJSON(msg); err != nil {
			break
		}
	}
	client.Conn.Close()
}

// ─────────────────────────── REST handlers ─────────────────────

func handleWalletAuth(c *fiber.Ctx) error {
	type Req struct {
		Address   string `json:"address"`
		Signature string `json:"signature"`
		Message   string `json:"message"`
	}
	req := new(Req)
	if err := c.BodyParser(req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
	}
	if !verifySignature(req.Address, req.Message, req.Signature) {
		return c.Status(401).JSON(fiber.Map{"error": "invalid signature"})
	}
	var player Player
	db.Where("wallet_address = ?", strings.ToLower(req.Address)).
		FirstOrCreate(&player, Player{WalletAddress: strings.ToLower(req.Address)})
	token, _ := generateJWT(player.WalletAddress)
	return c.JSON(fiber.Map{"token": token, "player": player})
}

func getPlayerProfile(c *fiber.Ctx) error {
	var player Player
	db.Where("wallet_address = ?", c.Locals("wallet")).First(&player)
	return c.JSON(player)
}

func updatePlayerProfile(c *fiber.Ctx) error {
	type Req struct{ Username *string `json:"username"` }
	req := new(Req)
	c.BodyParser(req)
	var player Player
	db.Where("wallet_address = ?", c.Locals("wallet")).First(&player)
	if req.Username != nil {
		player.Username = req.Username
	}
	db.Save(&player)
	return c.JSON(player)
}

func updatePlayerPosition(c *fiber.Ctx) error {
	type Req struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	req := new(Req)
	c.BodyParser(req)
	db.Model(&Player{}).Where("wallet_address = ?", c.Locals("wallet")).Updates(map[string]interface{}{
		"position_x": req.X,
		"position_y": req.Y,
	})
	return c.JSON(fiber.Map{"success": true})
}

func getServerInfo(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"server_id":    1,
		"player_count": hub.count(),
		"max_players":  50,
		"status":       "online",
	})
}

func joinServer(c *fiber.Ctx) error {
	db.Model(&Player{}).Where("wallet_address = ?", c.Locals("wallet")).Updates(map[string]interface{}{
		"is_online": true,
		"last_seen": time.Now(),
	})
	return c.JSON(fiber.Map{"success": true})
}

func playerHeartbeat(c *fiber.Ctx) error {
	db.Model(&Player{}).Where("wallet_address = ?", c.Locals("wallet")).Updates(map[string]interface{}{
		"is_online": true,
		"last_seen": time.Now(),
	})
	return c.JSON(fiber.Map{"success": true})
}

func playerDisconnect(c *fiber.Ctx) error {
	db.Model(&Player{}).Where("wallet_address = ?", c.Locals("wallet")).Updates(map[string]interface{}{
		"is_online": false,
		"last_seen": time.Now(),
	})
	return c.JSON(fiber.Map{"success": true})
}

func getOnlinePlayers(c *fiber.Ctx) error {
	var players []Player
	db.Where("is_online = ? AND wallet_address != ?", true, c.Locals("wallet")).
		Select("wallet_address", "position_x", "position_y", "username").Find(&players)
	result := make([]fiber.Map, 0, len(players))
	for _, p := range players {
		username := p.WalletAddress[:6] + "..."
		if p.Username != nil {
			username = *p.Username
		}
		result = append(result, fiber.Map{
			"wallet":   p.WalletAddress,
			"x":        p.PositionX,
			"y":        p.PositionY,
			"username": username,
		})
	}
	return c.JSON(fiber.Map{"players": result})
}

func cleanupOfflinePlayers() {
	for {
		time.Sleep(30 * time.Second)
		cutoff := time.Now().Add(-2 * time.Minute)
		db.Model(&Player{}).
			Where("is_online = ? AND last_seen < ?", true, cutoff).
			Update("is_online", false)
		fmt.Println("🧹 Cleaned up stale online flags")
	}
}

// ─────────────────────────── Auth helpers ──────────────────────

func verifySignature(address, message, signature string) bool {
	msg := fmt.Sprintf("\x19Ethereum Signed Message:\n%d%s", len(message), message)
	hash := crypto.Keccak256Hash([]byte(msg))
	sig, err := hexutil.Decode(signature)
	if err != nil || len(sig) != 65 {
		return false
	}
	if sig[64] >= 27 {
		sig[64] -= 27
	}
	pubKey, err := crypto.SigToPub(hash.Bytes(), sig)
	if err != nil || pubKey == nil {
		return false
	}
	return strings.EqualFold(crypto.PubkeyToAddress(*pubKey).Hex(), address)
}

func generateJWT(wallet string) (string, error) {
	claims := Claims{
		WalletAddress: wallet,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(jwtSecret))
}

func verifyJWT(tokenString string) (string, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		return []byte(jwtSecret), nil
	})
	if err != nil || !token.Valid {
		return "", err
	}
	return token.Claims.(*Claims).WalletAddress, nil
}

func authMiddleware(c *fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return c.Status(401).JSON(fiber.Map{"error": "missing auth"})
	}
	token, err := jwt.ParseWithClaims(strings.TrimPrefix(authHeader, "Bearer "), &Claims{}, func(t *jwt.Token) (interface{}, error) {
		return []byte(jwtSecret), nil
	})
	if err != nil || !token.Valid {
		return c.Status(401).JSON(fiber.Map{"error": "invalid token"})
	}
	c.Locals("wallet", token.Claims.(*Claims).WalletAddress)
	return c.Next()
}