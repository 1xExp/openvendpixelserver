package main

import (
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/contrib/websocket"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/joho/godotenv"
	gormPostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"game-server/utils"
	ws "game-server/websocket"
)

const DEBUG = true

func dbg(format string, args ...interface{}) {
	if DEBUG {
		log.Printf("[DBG] "+format, args...)
	}
}

type Player struct {
	ID            uint      `gorm:"primarykey" json:"id"`
	WalletAddress string    `gorm:"unique;not null" json:"wallet_address"`
	Username      *string   `gorm:"unique" json:"username"`
	DisplayName   *string   `json:"display_name"`
	Level         int       `gorm:"default:1" json:"level"`
	Experience    int       `gorm:"default:0" json:"experience"`
	Gold          int       `gorm:"default:0" json:"gold"`
	PositionX     float64   `gorm:"default:100" json:"position_x"`
	PositionY     float64   `gorm:"default:100" json:"position_y"`
	EquippedItem  string    `gorm:"default:''" json:"equipped_item"`
	IsChopping    bool      `gorm:"default:false" json:"is_chopping"`
	IsOnline      bool      `gorm:"default:false" json:"is_online"`
	LastSeen      time.Time `json:"last_seen"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type PlayerInventory struct {
	ID     uint   `gorm:"primarykey" json:"id"`
	Wallet string `gorm:"not null;index" json:"wallet"`
	ItemID string `gorm:"not null" json:"item_id"`
	Count  int    `gorm:"default:0" json:"count"`
}

type TreeState struct {
	TreeID      string         `json:"tree_id"`
	State       string         `json:"state"`
	MaxHP       int            `json:"max_hp"`
	CurrentHP   int            `json:"current_hp"`
	DamageMap   map[string]int `json:"damage_map"` // wallet -> total damage
	RespawnTime time.Time      `json:"respawn_time"`
}

var (
	db         *gorm.DB
	jwtSecret  string
	hub        = ws.NewHub()
	treesState = make(map[string]*TreeState)
	treesLock  sync.RWMutex
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file, reading from environment")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	jwtSecret = os.Getenv("JWT_SECRET")
	if databaseURL == "" || jwtSecret == "" {
		log.Fatal("DATABASE_URL and JWT_SECRET are required")
	}

	var err error
	db, err = gorm.Open(gormPostgres.Open(databaseURL), &gorm.Config{PrepareStmt: false})
	if err != nil {
		log.Fatal("failed to connect to database:", err)
	}
	db.AutoMigrate(&Player{}, &PlayerInventory{})
	log.Println("connected to postgresql")

	db.Model(&Player{}).Where("is_online = ?", true).Updates(map[string]interface{}{
		"is_online":   false,
		"is_chopping": false,
	})

	ws.CleanupOfflinePlayers(30*time.Second, func() {
		cutoff := time.Now().Add(-2 * time.Minute)
		db.Model(&Player{}).Where("is_online = ? AND last_seen < ?", true, cutoff).Updates(map[string]interface{}{
			"is_online":   false,
			"is_chopping": false,
		})
		dbg("stale online flags cleared")
	})

	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowHeaders: "Origin, Content-Type, Accept, Authorization",
	}))

	app.Post("/auth/wallet", handleWalletAuth)
	app.Get("/player/me", authMiddleware, getPlayerProfile)
	app.Put("/player/me", authMiddleware, updatePlayerProfile)
	app.Post("/player/position", authMiddleware, updatePlayerPosition)
	app.Post("/player/equipment", authMiddleware, updateEquipment)
	app.Get("/player/inventory", authMiddleware, getInventory)
	app.Get("/server/info", getServerInfo)
	app.Post("/server/join", authMiddleware, joinServer)
	app.Post("/player/heartbeat", authMiddleware, playerHeartbeat)
	app.Post("/player/disconnect", authMiddleware, playerDisconnect)
	app.Get("/players/online", authMiddleware, getOnlinePlayers)
	app.Get("/username/check", checkUsername)
	app.Post("/username/set", authMiddleware, setUsername)
	app.Put("/username/display", authMiddleware, updateDisplayName)
	app.Post("/tree/start-chop", authMiddleware, startChoppingTree)
	app.Post("/tree/chop-tick", authMiddleware, chopTreeTick)
	app.Post("/tree/finish-chop", authMiddleware, finishChoppingTree)
	app.Get("/tree/state", authMiddleware, getTreeState)

	app.Use("/ws", func(c *fiber.Ctx) error {
		token := c.Query("token")
		if token == "" {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "missing token"})
		}
		wallet, err := utils.VerifyJWT(token, jwtSecret)
		if err != nil {
			return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{"error": "invalid token"})
		}
		c.Locals("wallet", wallet)
		if websocket.IsWebSocketUpgrade(c) {
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})

	app.Get("/ws", websocket.New(handleFiberWebSocket))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("server running on :%s\n", port)
	log.Fatal(app.Listen(":" + port))
}

func resolveDisplayName(p *Player) string {
	if p.DisplayName != nil {
		return *p.DisplayName
	}
	if p.Username != nil {
		return *p.Username
	}
	return p.WalletAddress[:6] + "..."
}

func handleFiberWebSocket(c *websocket.Conn) {
	wallet := c.Locals("wallet").(string)

	client := &ws.WSClient{
		Conn:   c,
		Wallet: wallet,
		Send:   make(chan ws.WSMessage, 256),
	}

	hub.Register(wallet, client)
	dbg("ws connected: %s (total: %d)", wallet, hub.Count())

	go func() {
		players := hub.SnapshotPlayers(wallet, func(w string) (map[string]interface{}, error) {
			var p Player
			if err := db.Where("wallet_address = ?", w).First(&p).Error; err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"wallet":        w,
				"x":             p.PositionX,
				"y":             p.PositionY,
				"username":      resolveDisplayName(&p),
				"equipped_item": p.EquippedItem,
				"is_chopping":   p.IsChopping,
			}, nil
		})
		hub.Send(wallet, ws.WSMessage{
			Type: "init_players",
			Data: map[string]interface{}{"players": players},
		})
		dbg("init_players sent to %s (%d players)", wallet, len(players))
	}()

	go func() {
		var p Player
		db.Where("wallet_address = ?", wallet).First(&p)
		hub.Broadcast(ws.WSMessage{
			Type: "player_join",
			Data: map[string]interface{}{
				"wallet":        wallet,
				"x":             p.PositionX,
				"y":             p.PositionY,
				"username":      resolveDisplayName(&p),
				"equipped_item": p.EquippedItem,
				"is_chopping":   p.IsChopping,
			},
		}, wallet)
		dbg("player_join broadcast: %s", wallet)
	}()

	go ws.ClientWriter(client)
	ws.ClientReader(client, hub,
		func(w string) (float64, float64, error) {
			var p Player
			if err := db.Select("position_x, position_y").Where("wallet_address = ?", w).First(&p).Error; err != nil {
				return 0, 0, err
			}
			return p.PositionX, p.PositionY, nil
		},
		func(w string, x, y float64) {
			db.Model(&Player{}).Where("wallet_address = ?", w).Updates(map[string]interface{}{
				"position_x": x,
				"position_y": y,
				"last_seen":  time.Now(),
			})
		},
		func(w string) (string, error) {
			var p Player
			if err := db.Where("wallet_address = ?", w).First(&p).Error; err != nil {
				return "", err
			}
			return resolveDisplayName(&p), nil
		},
		func(w string) {
			db.Model(&Player{}).Where("wallet_address = ?", w).Updates(map[string]interface{}{
				"is_online":   false,
				"is_chopping": false,
				"last_seen":   time.Now(),
			})
		},
		func(w string) {
			db.Model(&Player{}).Where("wallet_address = ?", w).Update("last_seen", time.Now())
		},
	)
}

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
	if !utils.VerifySignature(req.Address, req.Message, req.Signature) {
		return c.Status(401).JSON(fiber.Map{"error": "invalid signature"})
	}
	var player Player
	db.Where("wallet_address = ?", strings.ToLower(req.Address)).
		FirstOrCreate(&player, Player{WalletAddress: strings.ToLower(req.Address)})
	token, _ := utils.GenerateJWT(player.WalletAddress, jwtSecret)
	dbg("auth success: %s", player.WalletAddress)
	return c.JSON(fiber.Map{"token": token, "player": player})
}

func getPlayerProfile(c *fiber.Ctx) error {
	var player Player
	db.Where("wallet_address = ?", c.Locals("wallet")).First(&player)
	return c.JSON(player)
}

func updatePlayerProfile(c *fiber.Ctx) error {
	type Req struct {
		Username *string `json:"username"`
	}
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

func updateEquipment(c *fiber.Ctx) error {
	wallet := c.Locals("wallet").(string)
	type Req struct {
		EquippedItem string `json:"equipped_item"`
	}
	req := new(Req)
	if err := c.BodyParser(req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
	}
	db.Model(&Player{}).Where("wallet_address = ?", wallet).Update("equipped_item", req.EquippedItem)
	hub.Broadcast(ws.WSMessage{
		Type: "player_equip",
		Data: map[string]interface{}{
			"wallet":        wallet,
			"equipped_item": req.EquippedItem,
		},
	}, "")
	dbg("equipment updated: %s -> %s", wallet, req.EquippedItem)
	return c.JSON(fiber.Map{"success": true})
}

func getInventory(c *fiber.Ctx) error {
	wallet := c.Locals("wallet").(string)
	var items []PlayerInventory
	if err := db.Where("wallet = ?", wallet).Find(&items).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "failed to get inventory"})
	}
	result := make([]fiber.Map, 0, len(items))
	for _, item := range items {
		result = append(result, fiber.Map{
			"item_id": item.ItemID,
			"count":   item.Count,
		})
	}
	return c.JSON(fiber.Map{"inventory": result})
}

func getServerInfo(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"server_id":    1,
		"player_count": hub.Count(),
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
		"is_online":   false,
		"is_chopping": false,
		"last_seen":   time.Now(),
	})
	return c.JSON(fiber.Map{"success": true})
}

func getOnlinePlayers(c *fiber.Ctx) error {
	var players []Player
	db.Where("is_online = ? AND wallet_address != ?", true, c.Locals("wallet")).
		Select("wallet_address", "position_x", "position_y", "username", "display_name", "equipped_item", "is_chopping").
		Find(&players)
	result := make([]fiber.Map, 0, len(players))
	for _, p := range players {
		result = append(result, fiber.Map{
			"wallet":        p.WalletAddress,
			"x":             p.PositionX,
			"y":             p.PositionY,
			"username":      resolveDisplayName(&p),
			"equipped_item": p.EquippedItem,
			"is_chopping":   p.IsChopping,
		})
	}
	return c.JSON(fiber.Map{"players": result})
}

func startChoppingTree(c *fiber.Ctx) error {
	wallet := c.Locals("wallet").(string)
	type Req struct {
		TreeID string `json:"tree_id"`
	}
	req := new(Req)
	if err := c.BodyParser(req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
	}

	treesLock.Lock()
	defer treesLock.Unlock()

	tree, exists := treesState[req.TreeID]
	if !exists {
		tree = &TreeState{
			TreeID:    req.TreeID,
			State:     "idle",
			MaxHP:     100,
			CurrentHP: 100,
			DamageMap: make(map[string]int),
		}
		treesState[req.TreeID] = tree
	}

	if tree.State == "stump" {
		if time.Now().Before(tree.RespawnTime) {
			return c.Status(400).JSON(fiber.Map{
				"error":      "tree is a stump",
				"respawn_at": tree.RespawnTime.Unix(),
			})
		}
		// Sudah bisa respawn
		tree.State = "idle"
		tree.CurrentHP = tree.MaxHP
		tree.DamageMap = make(map[string]int)
	}

	// Izinkan join baik saat idle maupun chopping
	tree.State = "chopping"
	if _, ok := tree.DamageMap[wallet]; !ok {
		tree.DamageMap[wallet] = 0
	}

	db.Model(&Player{}).Where("wallet_address = ?", wallet).Update("is_chopping", true)

	hub.Broadcast(ws.WSMessage{
		Type: "tree_state",
		Data: map[string]interface{}{"tree_id": req.TreeID, "state": "chopping"},
	}, "")

	dbg("chop joined: %s on tree %s (hp=%d/%d, choppers=%d)",
		wallet, req.TreeID, tree.CurrentHP, tree.MaxHP, len(tree.DamageMap))
	return c.JSON(fiber.Map{
		"success":    true,
		"current_hp": tree.CurrentHP,
		"max_hp":     tree.MaxHP,
	})
}

func chopTreeTick(c *fiber.Ctx) error {
	wallet := c.Locals("wallet").(string)
	type Req struct {
		TreeID string `json:"tree_id"`
		Damage int    `json:"damage"` // per-hit damage from client
	}
	req := new(Req)
	if err := c.BodyParser(req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
	}

	// Server-side validate damage (anti-cheat: max damage per tick = 20)
	damage := req.Damage
	if damage <= 0 || damage > 20 {
		damage = 10
	}
	dbg("chop-tick: wallet=%s tree=%s damage=%d", wallet, req.TreeID, damage)

	treesLock.Lock()
	defer treesLock.Unlock()

	tree, exists := treesState[req.TreeID]
	if !exists || tree.State != "chopping" {
		return c.Status(400).JSON(fiber.Map{"error": "tree not being chopped"})
	}

	if _, ok := tree.DamageMap[wallet]; !ok {
		tree.DamageMap[wallet] = 0
	}

	tree.DamageMap[wallet] += damage
	tree.CurrentHP -= damage
	if tree.CurrentHP < 0 {
		tree.CurrentHP = 0
	}

	fell := tree.CurrentHP <= 0
	woodRewards := make(map[string]int)

	if fell {
		totalDamage := 0
		for _, d := range tree.DamageMap {
			totalDamage += d
		}
		for w, d := range tree.DamageMap {
			contrib := float64(d) / float64(totalDamage) * 100.0
			if contrib < 10.0 {
				dbg("skip reward: %s contrib=%.1f%%", w, contrib)
				continue
			}
			wood := int(contrib/100.0*5 + 0.5)
			if wood < 1 {
				wood = 1
			}
			woodRewards[w] = wood
			var inv PlayerInventory
			err := db.Where("wallet = ? AND item_id = ?", w, "wood1").First(&inv).Error
			if err == gorm.ErrRecordNotFound {
				inv = PlayerInventory{Wallet: w, ItemID: "wood1", Count: wood}
				db.Create(&inv)
			} else {
				db.Model(&inv).Update("count", inv.Count+wood)
			}
			db.Model(&Player{}).Where("wallet_address = ?", w).Update("is_chopping", false)
			dbg("reward: %s contrib=%.1f%% wood=%d", w, contrib, wood)
		}
		tree.State = "stump"
		tree.RespawnTime = time.Now().Add(10 * time.Second)
		tree.DamageMap = make(map[string]int)
		hub.Broadcast(ws.WSMessage{
			Type: "tree_state",
			Data: map[string]interface{}{
				"tree_id":    req.TreeID,
				"state":      "stump",
				"respawn_at": tree.RespawnTime.Unix(),
			},
		}, "")
		dbg("pine tree fell: %s", req.TreeID)
	}

	return c.JSON(fiber.Map{
		"damage":      damage,
		"current_hp":  tree.CurrentHP,
		"max_hp":      tree.MaxHP,
		"fell":        fell,
		"wood_reward": woodRewards[wallet],
	})
}

func finishChoppingTree(c *fiber.Ctx) error {
	wallet := c.Locals("wallet").(string)
	type Req struct {
		TreeID string `json:"tree_id"`
	}
	req := new(Req)
	if err := c.BodyParser(req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
	}

	treesLock.Lock()
	defer treesLock.Unlock()

	tree, exists := treesState[req.TreeID]
	if !exists {
		return c.Status(400).JSON(fiber.Map{"error": "tree not found"})
	}

	// Player stop chop sebelum pohon tumbang — hapus dari chopper list
	delete(tree.DamageMap, wallet)
	db.Model(&Player{}).Where("wallet_address = ?", wallet).Update("is_chopping", false)

	dbg("finish-chop: %s left tree %s (hp=%d)", wallet, req.TreeID, tree.CurrentHP)
	return c.JSON(fiber.Map{
		"success":    true,
		"tree_state": tree.State,
		"current_hp": tree.CurrentHP,
	})
}

func getTreeState(c *fiber.Ctx) error {
	treeID := c.Query("tree_id")
	if treeID == "" {
		return c.Status(400).JSON(fiber.Map{"error": "tree_id required"})
	}

	treesLock.Lock()
	defer treesLock.Unlock()

	tree, exists := treesState[treeID]
	if !exists {
		return c.JSON(fiber.Map{"tree_id": treeID, "state": "idle"})
	}

	if tree.State == "stump" && time.Now().After(tree.RespawnTime) {
		tree.State = "idle"
		tree.CurrentHP = tree.MaxHP
		tree.DamageMap = make(map[string]int)
		hub.Broadcast(ws.WSMessage{
			Type: "tree_state",
			Data: map[string]interface{}{
				"tree_id": treeID,
				"state":   "idle",
			},
		}, "")
		dbg("tree respawned: %s", treeID)
	}

	resp := fiber.Map{
		"tree_id":    treeID,
		"state":      tree.State,
		"current_hp": tree.CurrentHP,
		"max_hp":     tree.MaxHP,
	}
	if tree.State == "stump" {
		resp["respawn_at"] = tree.RespawnTime.Unix()
	}
	return c.JSON(resp)
}

func checkUsername(c *fiber.Ctx) error {
	username := c.Query("username")
	if username == "" {
		return c.Status(400).JSON(fiber.Map{"error": "username required"})
	}
	matched, _ := regexp.MatchString(`^[a-z0-9]{3,16}$`, username)
	if !matched {
		return c.JSON(fiber.Map{
			"available": false,
			"error":     "username must be lowercase letters and numbers only (3-16 chars)",
		})
	}
	var count int64
	db.Model(&Player{}).Where("username = ?", username).Count(&count)
	return c.JSON(fiber.Map{"available": count == 0, "username": username})
}

func setUsername(c *fiber.Ctx) error {
	wallet := c.Locals("wallet").(string)
	type Req struct {
		Username    string  `json:"username"`
		DisplayName *string `json:"display_name"`
	}
	req := new(Req)
	if err := c.BodyParser(req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
	}
	matched, _ := regexp.MatchString(`^[a-z0-9]{3,16}$`, req.Username)
	if !matched {
		return c.Status(400).JSON(fiber.Map{"error": "username must be lowercase letters and numbers only (3-16 chars)"})
	}
	if req.DisplayName != nil {
		matched, _ := regexp.MatchString(`^[A-Za-z0-9]{3,16}$`, *req.DisplayName)
		if !matched {
			return c.Status(400).JSON(fiber.Map{"error": "display_name must be letters and numbers only (3-16 chars)"})
		}
	}
	var player Player
	if err := db.Where("wallet_address = ?", wallet).First(&player).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "player not found"})
	}
	if player.Username != nil {
		return c.Status(400).JSON(fiber.Map{"error": "username already set (cannot change)"})
	}
	var count int64
	db.Model(&Player{}).Where("username = ?", req.Username).Count(&count)
	if count > 0 {
		return c.Status(409).JSON(fiber.Map{"error": "username already taken"})
	}
	if req.DisplayName != nil {
		db.Model(&Player{}).Where("display_name = ?", *req.DisplayName).Count(&count)
		if count > 0 {
			return c.Status(409).JSON(fiber.Map{"error": "display_name already taken"})
		}
	}
	player.Username = &req.Username
	if req.DisplayName != nil {
		player.DisplayName = req.DisplayName
	} else {
		player.DisplayName = &req.Username
	}
	if err := db.Save(&player).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "failed to save"})
	}
	dbg("username set: %s -> %s", wallet, req.Username)
	return c.JSON(fiber.Map{"success": true, "player": player})
}

func updateDisplayName(c *fiber.Ctx) error {
	wallet := c.Locals("wallet").(string)
	type Req struct {
		DisplayName string `json:"display_name"`
	}
	req := new(Req)
	if err := c.BodyParser(req); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "invalid request"})
	}
	matched, _ := regexp.MatchString(`^[A-Za-z0-9]{3,16}$`, req.DisplayName)
	if !matched {
		return c.Status(400).JSON(fiber.Map{"error": "display_name must be letters and numbers only (3-16 chars)"})
	}
	var player Player
	db.Where("wallet_address = ?", wallet).First(&player)
	var count int64
	db.Model(&Player{}).Where("display_name = ? AND wallet_address != ?", req.DisplayName, wallet).Count(&count)
	if count > 0 {
		return c.Status(409).JSON(fiber.Map{"error": "display_name already taken"})
	}
	player.DisplayName = &req.DisplayName
	db.Save(&player)
	dbg("display_name updated: %s -> %s", wallet, req.DisplayName)
	return c.JSON(fiber.Map{"success": true, "player": player})
}

func authMiddleware(c *fiber.Ctx) error {
	authHeader := c.Get("Authorization")
	if authHeader == "" {
		return c.Status(401).JSON(fiber.Map{"error": "missing auth"})
	}
	token, err := utils.ParseJWT(strings.TrimPrefix(authHeader, "Bearer "), jwtSecret)
	if err != nil {
		return c.Status(401).JSON(fiber.Map{"error": "invalid token"})
	}
	c.Locals("wallet", token)
	return c.Next()
}
