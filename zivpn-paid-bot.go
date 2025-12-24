package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ==========================================
// Constants & Configuration
// ==========================================

const (
	BotConfigFile = "/etc/zivpn/bot-config.json"
	ApiPortFile   = "/etc/zivpn/api_port"
	ApiKeyFile    = "/etc/zivpn/apikey"
	DomainFile    = "/etc/zivpn/domain"
	PortFile	  = "/etc/zivpn/port"
	WalletFile    = "/etc/zivpn/wallets.json"
	MetricsFile   = "/etc/zivpn/metrics.json"
)

var ApiUrl = "http://127.0.0.1:" + PortFile + "/api"

var ApiKey = "AutoFtBot-agskjgdvsbdreiWG1234512SDKrqw"

type BotConfig struct {
	BotToken      string `json:"bot_token"`
	AdminID        int64  `json:"admin_id"`
	Mode           string `json:"mode"`
	Domain         string `json:"domain"`
	PakasirSlug    string `json:"pakasir_slug"`
	PakasirApiKey  string `json:"pakasir_api_key"`
	DailyPrice     int    `json:"daily_price"`
}

type IpInfo struct {
	City string `json:"city"`
	Isp  string `json:"isp"`
}

type UserData struct {
	Password string `json:"password"`
	Expired  string `json:"expired"`
	Status   string `json:"status"`
}

type WalletEntry struct {
	TelegramID int64 `json:"telegram_id"`
	Balance    int   `json:"balance"`
	TrialUsed   bool `json:"trial_used"`
	PendingPassword string `json:"pending_password,omitempty"`
	PendingDays     int    `json:"pending_days,omitempty"`
	CreatedCount int    `json:"created_count,omitempty"`
	Banned       bool   `json:"banned,omitempty"`
}

// ==========================================
// Global State
// ==========================================

var userStates = make(map[int64]string)
var tempUserData = make(map[int64]map[string]string)
var lastMessageIDs = make(map[int64]int)
var mutex = &sync.Mutex{}

// ==========================================
// Main Entry Point
// ==========================================

func main() {
	if keyBytes, err := ioutil.ReadFile(ApiKeyFile); err == nil {
		ApiKey = strings.TrimSpace(string(keyBytes))
	}

	// Load API Port
	if portBytes, err := ioutil.ReadFile(ApiPortFile); err == nil {
		port := strings.TrimSpace(string(portBytes))
		ApiUrl = fmt.Sprintf("http://127.0.0.1:%s/api", port)
	}

	config, err := loadConfig()
	if err != nil {
		log.Fatal("Gagal memuat konfigurasi bot:", err)
	}

	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	// Start Payment Checker
	go startPaymentChecker(bot, &config)

	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, update.Message, &config)
		} else if update.CallbackQuery != nil {
			handleCallback(bot, update.CallbackQuery, &config)
		}
	}
}

// ==========================================
// Telegram Event Handlers
// ==========================================

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	// In Paid Bot, everyone can access, but actions are restricted/paid
	// Admin still has full control

	if state, exists := userStates[msg.From.ID]; exists {
		handleState(bot, msg, state, config)
		return
	}

	// Handle Document Upload (Restore) - Admin Only
	if msg.Document != nil && msg.From.ID == config.AdminID {
		if state, exists := userStates[msg.From.ID]; exists && state == "waiting_restore_file" {
			processRestoreFile(bot, msg, config)
			return
		}
	}

	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			showMainMenu(bot, msg.Chat.ID, config, msg.From.ID)
		default:
			replyError(bot, msg.Chat.ID, "Perintah tidak dikenal.")
		}
	}
}

func handleCallback(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, config *BotConfig) {
	chatID := query.Message.Chat.ID
	userID := query.From.ID

	switch {
	case query.Data == "menu_create":
		startCreateUser(bot, chatID, userID)
	case query.Data == "menu_info":
		systemInfo(bot, chatID, config)
	case query.Data == "cancel":
		cancelOperation(bot, chatID, userID, config)

	// New Paid Menu handlers
	case query.Data == "menu_trial":
		startTrial(bot, chatID, userID)
	case query.Data == "menu_renew":
		startRenew(bot, chatID, userID)
	case query.Data == "menu_list":
		listAccounts(bot, chatID)
	case query.Data == "menu_topup":
		startTopup(bot, chatID, userID)

	case query.Data == "menu_admin":
		if userID == config.AdminID {
			showBackupRestoreMenu(bot, chatID)
		}
	case query.Data == "menu_admin_manage":
		if userID == config.AdminID {
			showAdminManageMenu(bot, chatID)
		}
	case query.Data == "admin_add_balance":
		if userID == config.AdminID {
			userStates[userID] = "admin_add_balance_input"
			sendMessage(bot, chatID, "🟢 Masukkan TelegramID dan jumlah untuk ditambahkan (contoh: 7251232303 50000):")
		}
	case query.Data == "admin_remove_balance":
		if userID == config.AdminID {
			userStates[userID] = "admin_remove_balance_input"
			sendMessage(bot, chatID, "🔴 Masukkan TelegramID dan jumlah untuk dikurangkan (contoh: 7251232303 50000):")
		}
	case query.Data == "admin_ban":
		if userID == config.AdminID {
			userStates[userID] = "admin_ban_input"
			sendMessage(bot, chatID, "⛔ Masukkan TelegramID untuk diban (contoh: 7251232303):")
		}
	case query.Data == "admin_unban":
		if userID == config.AdminID {
			userStates[userID] = "admin_unban_input"
			sendMessage(bot, chatID, "✅ Masukkan TelegramID untuk di-unban (contoh: 7251232303):")
		}
	case query.Data == "admin_view_activity":
		if userID == config.AdminID {
			today, week, month, _ := computeMetrics()
			sendMessage(bot, chatID, fmt.Sprintf("📈 Aktivitas: Hari ini %d • Minggu ini %d • Bulan ini %d", today, week, month))
		}
	case query.Data == "admin_forward_mode":
		if userID == config.AdminID {
			userStates[userID] = "admin_forward_mode"
			sendMessage(bot, chatID, "📨 Silakan forward pesan dari pengguna ke chat ini. Setelah diterima, Anda akan diberi opsi tindakan.")
		}
	case query.Data == "menu_admin_create_free":
		if userID == config.AdminID {
			startAdminCreateFree(bot, chatID, userID, config)
		}
	case query.Data == "menu_backup_action":
		if userID == config.AdminID {
			performBackup(bot, chatID)
		}
	case query.Data == "menu_restore_action":
		if userID == config.AdminID {
			startRestore(bot, chatID, userID)
		}
	}

	bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string, config *BotConfig) {
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)
	chatID := msg.Chat.ID

	switch state {
	case "create_password":
		if !validatePassword(bot, chatID, text) {
			return
		}
		mutex.Lock()
		tempUserData[userID]["password"] = text
		mutex.Unlock()
		userStates[userID] = "create_days"
		sendMessage(bot, chatID, fmt.Sprintf("⏳ Masukkan Durasi (hari)\nHarga: Rp %d / hari:", config.DailyPrice))

	case "create_days":
		days, ok := validateNumber(bot, chatID, text, 1, 365, "Durasi")
		if !ok {
			return
		}
		mutex.Lock()
		tempUserData[userID]["days"] = text
		mutex.Unlock()

		// Create account via balance deduction (Topup model)
		required := days * config.DailyPrice
		balance := getBalance(userID)
		if balance < required {
			sendMessage(bot, chatID, fmt.Sprintf("⚠️ Saldo Anda: Rp %d. Diperlukan Rp %d untuk membuat akun %d hari. Silakan Topup minimal Rp 5000.", balance, required, days))
			// Store attempted purchase in wallet so it can be completed after topup
			if err := setPendingPurchase(userID, tempUserData[userID]["password"], days); err != nil {
				log.Printf("Failed to set pending purchase for %d: %v", userID, err)
			}
			resetState(userID)
			return
		}

		// Deduct and create
		if err := deductBalance(userID, required); err != nil {
			replyError(bot, chatID, "Gagal memproses saldo: "+err.Error())
			resetState(userID)
			return
		}
		password := tempUserData[userID]["password"]
		createUser(bot, chatID, userID, password, days, config)
		delete(tempUserData, userID)
		delete(userStates, userID)
    
	// Trial flow: ask for password, then create with 1 day
	case "trial_password":
		if !validatePassword(bot, chatID, text) {
			return
		}
		// Enforce trial policy: if user has zero balance, allow only once
		// Admin users are allowed unlimited trials
		if msg.From.ID != config.AdminID {
			if getBalance(userID) == 0 {
				if hasUsedTrial(userID) {
					sendMessage(bot, chatID, "⚠️ Anda sudah menggunakan trial. Untuk mendapatkan trial lagi, silakan Topup minimal Rp 5000.")
					resetState(userID)
					return
				}
			}
		}
		password := text
		// Create trial for 1 day
		res, err := apiCall("POST", "/user/create", map[string]interface{}{
			"password": password,
			"days":     1,
		})
		if err != nil {
			replyError(bot, chatID, "Error API: "+err.Error())
			resetState(userID)
			return
		}
		if res["success"] == true {
			// mark trial used only if user had zero balance
			if getBalance(userID) == 0 {
				markTrialUsed(userID)
			}
			data := res["data"].(map[string]interface{})
			resetState(userID)
			sendAccountInfo(bot, chatID, data, config)
		} else {
			resetState(userID)
			replyError(bot, chatID, fmt.Sprintf("Gagal membuat trial: %s", res["message"]))
		}

	// Renew flow
	case "renew_password":
		mutex.Lock()
		tempUserData[userID] = make(map[string]string)
		tempUserData[userID]["password"] = text
		tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
		mutex.Unlock()
		userStates[userID] = "renew_days"
		sendMessage(bot, chatID, "⏳ Masukkan Durasi Perpanjangan (hari):")

	case "renew_days":
		days, ok := validateNumber(bot, chatID, text, 1, 3650, "Durasi")
		if !ok {
			return
		}
		pwd := tempUserData[userID]["password"]
		// Renew via balance deduction
		required := days * config.DailyPrice
		if getBalance(userID) < required {
			sendMessage(bot, chatID, fmt.Sprintf("⚠️ Saldo tidak mencukupi. Diperlukan Rp %d. Silakan Topup minimal Rp 5000.", required))
			resetState(userID)
			return
		}
		// Deduct and call renew API
		if err := deductBalance(userID, required); err != nil {
			replyError(bot, chatID, "Gagal memproses saldo: "+err.Error())
			resetState(userID)
			return
		}
		res, err := apiCall("POST", "/user/renew", map[string]interface{}{
			"password": pwd,
			"days":     days,
		})
		resetState(userID)
		if err != nil {
			replyError(bot, chatID, "Error API: "+err.Error())
			return
		}
		if res["success"] == true {
			data := res["data"].(map[string]interface{})
			sendMessage(bot, chatID, fmt.Sprintf("✅ User %s berhasil diperpanjang. Expired: %s\nSaldo tersisa: Rp %d", data["password"], data["expired"], getBalance(userID)))
			showMainMenu(bot, chatID, config, userID)
		} else {
			replyError(bot, chatID, fmt.Sprintf("Gagal memperpanjang: %s", res["message"]))
		}

	// Admin create free flow
	case "admin_create_password":
		if msg.From.ID != config.AdminID {
			replyError(bot, chatID, "Hanya admin yang dapat melakukan ini.")
			resetState(userID)
			return
		}
		if !validatePassword(bot, chatID, text) {
			return
		}
		mutex.Lock()
		tempUserData[userID] = make(map[string]string)
		tempUserData[userID]["password"] = text
		tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
		mutex.Unlock()
		userStates[userID] = "admin_create_days"
		sendMessage(bot, chatID, "⏳ Masukkan Durasi (hari) untuk akun gratis:")

	case "admin_create_days":
		if msg.From.ID != config.AdminID {
			replyError(bot, chatID, "Hanya admin yang dapat melakukan ini.")
			resetState(userID)
			return
		}
		days, ok := validateNumber(bot, chatID, text, 1, 3650, "Durasi")
		if !ok {
			return
		}
		pwd := tempUserData[userID]["password"]
		res, err := apiCall("POST", "/user/create", map[string]interface{}{
			"password": pwd,
			"days":     days,
		})
		resetState(userID)
		if err != nil {
			replyError(bot, chatID, "Error API: "+err.Error())
			return
		}
		if res["success"] == true {
			data := res["data"].(map[string]interface{})
			sendMessage(bot, chatID, fmt.Sprintf("✅ Akun gratis dibuat: %s\nExpired: %s", data["password"], data["expired"]))
			showMainMenu(bot, chatID, config, userID)
		} else {
			replyError(bot, chatID, fmt.Sprintf("Gagal membuat akun: %s", res["message"]))
		}

	case "admin_add_balance_input":
		if msg.From.ID != config.AdminID {
			replyError(bot, chatID, "Hanya admin yang dapat melakukan ini.")
			resetState(userID)
			return
		}
		parts := strings.Fields(text)
		if len(parts) != 2 {
			sendMessage(bot, chatID, "Format salah. Contoh: 7251232303 50000")
			return
		}
		tid, err1 := strconv.ParseInt(parts[0], 10, 64)
		amt, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			sendMessage(bot, chatID, "ID atau jumlah tidak valid.")
			resetState(userID)
			return
		}
		if err := addBalance(tid, amt); err != nil {
			replyError(bot, chatID, "Gagal menambah saldo: "+err.Error())
		} else {
			sendMessage(bot, chatID, fmt.Sprintf("✅ Berhasil menambah Rp %d ke user %d", amt, tid))
		}
		resetState(userID)

	case "admin_remove_balance_input":
		if msg.From.ID != config.AdminID {
			replyError(bot, chatID, "Hanya admin yang dapat melakukan ini.")
			resetState(userID)
			return
		}
		parts := strings.Fields(text)
		if len(parts) != 2 {
			sendMessage(bot, chatID, "Format salah. Contoh: 7251232303 50000")
			return
		}
		tid, err1 := strconv.ParseInt(parts[0], 10, 64)
		amt, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			sendMessage(bot, chatID, "ID atau jumlah tidak valid.")
			resetState(userID)
			return
		}
		if err := deductBalance(tid, amt); err != nil {
			replyError(bot, chatID, "Gagal mengurangi saldo: "+err.Error())
		} else {
			sendMessage(bot, chatID, fmt.Sprintf("✅ Berhasil mengurangi Rp %d dari user %d", amt, tid))
		}
		resetState(userID)

	case "admin_ban_input":
		if msg.From.ID != config.AdminID {
			replyError(bot, chatID, "Hanya admin yang dapat melakukan ini.")
			resetState(userID)
			return
		}
		tid, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			sendMessage(bot, chatID, "TelegramID tidak valid.")
			resetState(userID)
			return
		}
		wallets, _ := loadWallets()
		idx := getWalletIndex(wallets, tid)
		if idx == -1 {
			wallets = append(wallets, WalletEntry{TelegramID: tid, Balance: 0, TrialUsed: false, Banned: true})
		} else {
			wallets[idx].Banned = true
		}
		saveWallets(wallets)
		sendMessage(bot, chatID, fmt.Sprintf("⛔ User %d telah diban.", tid))
		resetState(userID)

	case "admin_unban_input":
		if msg.From.ID != config.AdminID {
			replyError(bot, chatID, "Hanya admin yang dapat melakukan ini.")
			resetState(userID)
			return
		}
		tid, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			sendMessage(bot, chatID, "TelegramID tidak valid.")
			resetState(userID)
			return
		}
		wallets, _ := loadWallets()
		idx := getWalletIndex(wallets, tid)
		if idx != -1 {
			wallets[idx].Banned = false
			saveWallets(wallets)
			sendMessage(bot, chatID, fmt.Sprintf("✅ User %d telah di-unban.", tid))
		} else {
			sendMessage(bot, chatID, "User tidak ditemukan di wallet.")
		}
		resetState(userID)

	case "admin_forward_mode":
		// Expect a forwarded message from admin
		if msg.ForwardFrom == nil {
			sendMessage(bot, chatID, "Silakan forward pesan dari pengguna yang ingin Anda tindak lanjuti.")
			return
		}
		orig := msg.ForwardFrom.ID
		// store target and any text
		mutex.Lock()
		tempUserData[userID] = make(map[string]string)
		tempUserData[userID]["forward_target"] = strconv.FormatInt(orig, 10)
		if msg.Text != "" {
			tempUserData[userID]["forward_text"] = msg.Text
		} else if msg.Caption != "" {
			tempUserData[userID]["forward_text"] = msg.Caption
		}
		mutex.Unlock()
		// prompt admin to type a message to send to the user
		userStates[userID] = "admin_forward_compose"
		sendMessage(bot, chatID, fmt.Sprintf("Forward diterima dari %d. Ketik pesan yang ingin Anda kirim ke pengguna ini:", orig))

	case "admin_forward_compose":
		// admin types message to forward to previously forwarded user
		targetStr := ""
		if tmp, ok := tempUserData[userID]["forward_target"]; ok {
			targetStr = tmp
		}
		if targetStr == "" {
			sendMessage(bot, chatID, "Target tidak ditemukan. Mulai ulang mode forward.")
			resetState(userID)
			return
		}
		tid, _ := strconv.ParseInt(targetStr, 10, 64)
		// send the admin's text to the target user
		pm := tgbotapi.NewMessage(tid, text)
		if _, err := bot.Send(pm); err != nil {
			replyError(bot, chatID, "Gagal mengirim pesan ke user: "+err.Error())
		} else {
			sendMessage(bot, chatID, fmt.Sprintf("✅ Pesan terkirim ke %d", tid))
		}
		delete(tempUserData, userID)
		resetState(userID)

	case "topup_amount":
		// parse amount
		amt, err := strconv.Atoi(text)
		if err != nil || amt < 5000 {
			sendMessage(bot, chatID, "❌ Jumlah tidak valid. Minimal topup adalah Rp 5000. Silakan coba lagi:")
			return
		}
		// create pakasir order
		orderID := fmt.Sprintf("ZIVPN-TOPUP-%d-%d", userID, time.Now().Unix())
		payment, err := createPakasirTransaction(config, orderID, amt)
		if err != nil {
			replyError(bot, chatID, "Gagal membuat transaksi topup: "+err.Error())
			resetState(userID)
			return
		}
		// store order for checking
		mutex.Lock()
		tempUserData[userID]["order_id"] = orderID
		tempUserData[userID]["price"] = strconv.Itoa(amt)
		tempUserData[userID]["action"] = "topup"
		mutex.Unlock()

		qrUrl := fmt.Sprintf("https://api.qrserver.com/v1/create-qr-code/?size=300x300&data=%s", payment.PaymentNumber)
		msgText := fmt.Sprintf("💳 **Topup Saldo**\nJumlah: Rp %d\nSilakan scan QRIS di bawah untuk melakukan pembayaran.\nExpired: %s", amt, payment.ExpiredAt)
		photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(qrUrl))
		photo.Caption = msgText
		photo.ParseMode = "Markdown"
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel"),
			),
		)
		photo.ReplyMarkup = keyboard
		deleteLastMessage(bot, chatID)
		sentMsg, err := bot.Send(photo)
		if err == nil {
			lastMessageIDs[chatID] = sentMsg.MessageID
		}
		delete(userStates, userID)
	}
}

// ==========================================
// Feature Implementation
// ==========================================

func startCreateUser(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "create_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	mutex.Unlock()
	sendMessage(bot, chatID, "👤 Masukkan Password Baru:")
}

func processPayment(bot *tgbotapi.BotAPI, chatID int64, userID int64, days int, config *BotConfig) {
	// Now handled by wallet topup model. This function is deprecated.
	sendMessage(bot, chatID, "Fitur pembayaran langsung sudah digantikan oleh sistem Topup. Silakan gunakan menu Topup Saldo terlebih dahulu jika saldo tidak mencukupi.")
}

func startPaymentChecker(bot *tgbotapi.BotAPI, config *BotConfig) {
	ticker := time.NewTicker(1 * time.Minute)
	for range ticker.C {
		mutex.Lock()
		for userID, data := range tempUserData {
			if orderID, ok := data["order_id"]; ok {
				price := data["price"]
				chatID, _ := strconv.ParseInt(data["chat_id"], 10, 64)
				// Determine action (topup or account_purchase)
				action := data["action"]
				status, err := checkPakasirStatus(config, orderID, price)
				if err == nil && (status == "completed" || status == "success") {
					if action == "topup" {
						// Add balance to user's wallet
						amt, _ := strconv.Atoi(price)
						addBalance(userID, amt)
						current := getBalance(userID)
						sendMessage(bot, chatID, fmt.Sprintf("✅ Topup berhasil: Rp %d. Saldo Anda saat ini: Rp %d", amt, current))
						// If there is a pending purchase, try to complete it
						wallets, _ := loadWallets()
						idx := getWalletIndex(wallets, userID)
						if idx != -1 && wallets[idx].PendingPassword != "" && wallets[idx].PendingDays > 0 {
							required := wallets[idx].PendingDays * config.DailyPrice
							if current >= required {
								// deduct and create account
								deductBalance(userID, required)
								pw := wallets[idx].PendingPassword
								doDays := wallets[idx].PendingDays
								clearPendingPurchase(userID)
								createUser(bot, chatID, userID, pw, doDays, config)
								sendMessage(bot, chatID, fmt.Sprintf("✅ Pembelian otomatis selesai. Akun dibuat. Saldo tersisa: Rp %d", getBalance(userID)))
							}
						}
					} else if action == "buy_account" {
						password := data["password"]
						days, _ := strconv.Atoi(data["days"])
						// Deduct balance and create account
						required := days * config.DailyPrice
						if getBalance(userID) >= required {
							deductBalance(userID, required)
							// tempUserData stores strings, so password is already a string
							createUser(bot, chatID, userID, password, days, config)
						} else {
							sendMessage(bot, chatID, "Pembayaran berhasil, tetapi saldo tidak mencukupi untuk pemotongan. Silakan hubungi admin.")
						}
					}
					delete(tempUserData, userID)
					delete(userStates, userID)
				} else if err != nil {
					log.Printf("Error checking payment for %d: %v", userID, err)
				}
			}
		}
		mutex.Unlock()
	}
}

func createUser(bot *tgbotapi.BotAPI, chatID int64, ownerID int64, password string, days int, config *BotConfig) {
	res, err := apiCall("POST", "/user/create", map[string]interface{}{
		"password": password,
		"days":     days,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		// Track metrics and per-user created count using ownerID
		incrementCreatedCount(ownerID)
		appendMetric(ownerID)
		sendAccountInfo(bot, chatID, data, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal membuat akun: %s", res["message"]))
	}
}

// ==========================================
// Pakasir API
// ==========================================

type PakasirPayment struct {
	PaymentNumber string `json:"payment_number"`
	ExpiredAt     string `json:"expired_at"`
}

func createPakasirTransaction(config *BotConfig, orderID string, amount int) (*PakasirPayment, error) {
	url := fmt.Sprintf("https://app.pakasir.com/api/transactioncreate/qris")
	payload := map[string]interface{}{
		"project":  config.PakasirSlug,
		"order_id": orderID,
		"amount":   amount,
		"api_key":  config.PakasirApiKey,
	}

	jsonPayload, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if paymentData, ok := result["payment"].(map[string]interface{}); ok {
		return &PakasirPayment{
			PaymentNumber: paymentData["payment_number"].(string),
			ExpiredAt:     paymentData["expired_at"].(string),
		}, nil
	}
	return nil, fmt.Errorf("invalid response from Pakasir")
}

// -------------------- Wallet helpers --------------------
func loadWallets() ([]WalletEntry, error) {
	var wallets []WalletEntry
	data, err := ioutil.ReadFile(WalletFile)
	if err != nil {
		if os.IsNotExist(err) {
			return wallets, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &wallets); err != nil {
		return nil, err
	}
	return wallets, nil
}

func saveWallets(wallets []WalletEntry) error {
	data, err := json.MarshalIndent(wallets, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(WalletFile, data, 0644)
}

func getWalletIndex(wallets []WalletEntry, telegramID int64) int {
	for i, w := range wallets {
		if w.TelegramID == telegramID {
			return i
		}
	}
	return -1
}

func getBalance(telegramID int64) int {
	wallets, err := loadWallets()
	if err != nil {
		return 0
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		return 0
	}
	return wallets[idx].Balance
}

func addBalance(telegramID int64, amount int) error {
	wallets, err := loadWallets()
	if err != nil {
		return err
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		wallets = append(wallets, WalletEntry{TelegramID: telegramID, Balance: amount, TrialUsed: false})
	} else {
		wallets[idx].Balance += amount
	}
	return saveWallets(wallets)
}

func deductBalance(telegramID int64, amount int) error {
	wallets, err := loadWallets()
	if err != nil {
		return err
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		return nil
	}
	wallets[idx].Balance -= amount
	if wallets[idx].Balance < 0 {
		wallets[idx].Balance = 0
	}
	return saveWallets(wallets)
}

func hasUsedTrial(telegramID int64) bool {
	wallets, err := loadWallets()
	if err != nil {
		return false
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		return false
	}
	return wallets[idx].TrialUsed
}

func markTrialUsed(telegramID int64) error {
	wallets, err := loadWallets()
	if err != nil {
		return err
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		wallets = append(wallets, WalletEntry{TelegramID: telegramID, Balance: 0, TrialUsed: true})
	} else {
		wallets[idx].TrialUsed = true
	}
	return saveWallets(wallets)
}

func setPendingPurchase(telegramID int64, password string, days int) error {
	wallets, err := loadWallets()
	if err != nil {
		return err
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		wallets = append(wallets, WalletEntry{TelegramID: telegramID, Balance: 0, TrialUsed: false, PendingPassword: password, PendingDays: days})
	} else {
		wallets[idx].PendingPassword = password
		wallets[idx].PendingDays = days
	}
	return saveWallets(wallets)
}

func clearPendingPurchase(telegramID int64) error {
	wallets, err := loadWallets()
	if err != nil {
		return err
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		return nil
	}
	wallets[idx].PendingPassword = ""
	wallets[idx].PendingDays = 0
	return saveWallets(wallets)
}

func checkPakasirStatus(config *BotConfig, orderID string, amountStr string) (string, error) {
	url := fmt.Sprintf("https://app.pakasir.com/api/transactiondetail?project=%s&amount=%s&order_id=%s&api_key=%s",
		config.PakasirSlug, amountStr, orderID, config.PakasirApiKey)

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)

	if transaction, ok := result["transaction"].(map[string]interface{}); ok {
		return transaction["status"].(string), nil
	}
	return "", fmt.Errorf("transaction not found")
}

// -------------------- Metrics helpers --------------------
type MetricsEntry struct {
	Timestamp string `json:"timestamp"`
	Owner     int64  `json:"owner"`
}

func appendMetric(owner int64) error {
	var entries []MetricsEntry
	data, err := ioutil.ReadFile(MetricsFile)
	if err == nil {
		json.Unmarshal(data, &entries)
	}
	entries = append(entries, MetricsEntry{Timestamp: time.Now().Format(time.RFC3339), Owner: owner})
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(MetricsFile, out, 0644)
}

func computeMetrics() (today int, week int, month int, err error) {
	entries := []MetricsEntry{}
	data, err := ioutil.ReadFile(MetricsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, 0, nil
		}
		return 0, 0, 0, err
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return 0, 0, 0, err
	}

	now := time.Now()
	year, weeknum := now.ISOWeek()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	for _, e := range entries {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
			today++
		}
		y, w := t.ISOWeek()
		if y == year && w == weeknum {
			week++
		}
		if !t.Before(monthStart) {
			month++
		}
	}
	return today, week, month, nil
}

func incrementCreatedCount(telegramID int64) error {
	wallets, err := loadWallets()
	if err != nil {
		return err
	}
	idx := getWalletIndex(wallets, telegramID)
	if idx == -1 {
		wallets = append(wallets, WalletEntry{TelegramID: telegramID, Balance: 0, TrialUsed: false, CreatedCount: 1})
	} else {
		wallets[idx].CreatedCount++
	}
	return saveWallets(wallets)
}

// ==========================================
// UI & Helpers (Simplified for Paid Bot)
// ==========================================

func getDisplayName(bot *tgbotapi.BotAPI, userID int64) string {
	if userID == 0 {
		return ""
	}

	// Avoid calling bot.GetChat to remain compatible with different
	// versions of the telegram library on target systems. Return a
	// simple fallback display name based on the Telegram ID.
	return fmt.Sprintf("Pengguna %d", userID)
}

func showMainMenu(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig, requesterID ...int64) {
	ipInfo, _ := getIpInfo()
	domain := config.Domain
	if domain == "" {
		domain = "(Not Configured)"
	}

	// Accept optional requesterID so admin buttons still appear even in group chats.
	isAdmin := chatID == config.AdminID
	if len(requesterID) > 0 && requesterID[0] == config.AdminID {
		isAdmin = true
	}

	// Greeting and stats
	var userName string
	var userID int64 = 0
	if len(requesterID) > 0 {
		userID = requesterID[0]
		userName = getDisplayName(bot, userID)
	}
	if userName == "" {
		userName = "Pengguna"
	}

	balance := 0
	created := 0
	if userID != 0 {
		balance = getBalance(userID)
		wallets, _ := loadWallets()
		idx := getWalletIndex(wallets, userID)
		if idx != -1 {
			created = wallets[idx].CreatedCount
		}
	}

	todayCount, weekCount, monthCount, _ := computeMetrics()

			msgText := fmt.Sprintf(`━━━━━━━━━━━━━━━━━━━━━
		RyyStore Zivpn UDP
	━━━━━━━━━━━━━━━━━━━━━
	Hai %s
	Saldo Anda : Rp %d
	Akun dibuat oleh Anda : %d
	Statistik : Hari ini %d • Minggu ini %d • Bulan ini %d
	━━━━━━━━━━━━━━━━━━━━━
	 • Domain   : %s
	 • City     : %s
	 • ISP      : %s
	 • Harga    : Rp %d / Hari
	━━━━━━━━━━━━━━━━━━━━━

	Credit: [RyyStorevp1](https://t.me/RyyStorevp1)
	Bot: [%s](https://t.me/%s)`, userName, balance, created, todayCount, weekCount, monthCount, domain, ipInfo.City, ipInfo.Isp, config.DailyPrice, bot.Self.UserName, bot.Self.UserName)

	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ParseMode = "Markdown"

	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛒 Beli Akun Premium", "menu_create"),
			tgbotapi.NewInlineKeyboardButtonData("💳 Topup Saldo", "menu_topup"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🆓 Trial Akun", "menu_trial"),
			tgbotapi.NewInlineKeyboardButtonData("🔁 Renew Akun", "menu_renew"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📋 List Akun", "menu_list"),
		),
	}

	if isAdmin {
		rows = append(rows,
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("📊 System Info", "menu_info")),
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("🛠️ Admin Panel", "menu_admin")),
		)
	}

	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	sendAndTrack(bot, msg)
}

func sendAccountInfo(bot *tgbotapi.BotAPI, chatID int64, data map[string]interface{}, config *BotConfig) {
	domain := config.Domain
	if domain == "" {
		domain = "(Not Configured)"
	}

	// Prefer API-provided fields if available; avoid showing server IP
	pwd := data["password"]
	exp := data["expired"]

	msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n  PREMIUM ACCOUNT\n━━━━━━━━━━━━━━━━━━━━━\nPassword   : %s\nDomain     : %s\nExpired On : %s\n━━━━━━━━━━━━━━━━━━━━━\n```\nTerima kasih telah berlangganan!",
		pwd, domain, exp,
	)

	reply := tgbotapi.NewMessage(chatID, msg)
	reply.ParseMode = "Markdown"
	deleteLastMessage(bot, chatID)
	bot.Send(reply)
	showMainMenu(bot, chatID, config, chatID)
}

// Start trial flow: ask for desired password then create a 1-day account
func startTrial(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "trial_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	mutex.Unlock()
	sendMessage(bot, chatID, "🆓 Trial Akun\nSilakan masukkan password yang diinginkan (3-20 karakter):")
}

// Start renew flow: ask for password then duration
func startRenew(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "renew_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	mutex.Unlock()
	sendMessage(bot, chatID, "🔁 Renew Akun\nSilakan masukkan password akun yang ingin diperpanjang:")
}

// List accounts: call API and show a formatted list
func listAccounts(bot *tgbotapi.BotAPI, chatID int64) {
	res, err := apiCall("GET", "/users", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}
	if res["success"] != true {
		replyError(bot, chatID, fmt.Sprintf("Gagal mengambil daftar: %v", res["message"]))
		return
	}
	data := res["data"]
	usersArr, ok := data.([]interface{})
	if !ok {
		replyError(bot, chatID, "Format data tidak sesuai dari API")
		return
	}

	if len(usersArr) == 0 {
		sendMessage(bot, chatID, "📋 Daftar akun kosong.")
		return
	}

	var b strings.Builder
	b.WriteString("📋 Daftar Akun:\n")
	for i, u := range usersArr {
		if i >= 200 { // safety cap
			b.WriteString("\n...dan masih banyak lagi...\n")
			break
		}
		m, ok := u.(map[string]interface{})
		if !ok {
			continue
		}
		pwd := m["password"]
		exp := m["expired"]
		status := m["status"]
		b.WriteString(fmt.Sprintf("%d. %s — Exp: %s — %s\n", i+1, pwd, exp, status))
	}

	sendMessage(bot, chatID, b.String())
}

// Admin: start free-account creation flow
func startAdminCreateFree(bot *tgbotapi.BotAPI, chatID int64, userID int64, config *BotConfig) {
	if userID != config.AdminID {
		replyError(bot, chatID, "Hanya admin yang dapat melakukan ini.")
		return
	}
	userStates[userID] = "admin_create_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	mutex.Unlock()
	sendMessage(bot, chatID, "➕ Buat Akun Gratis\nMasukkan password untuk akun baru (3-20 karakter):")
}

// ---------------- Topup flow ----------------
func startTopup(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "topup_amount"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	mutex.Unlock()
	sendMessage(bot, chatID, "💳 Topup Saldo\nMasukkan jumlah topup minimal Rp 5000 (contoh: 5000):")
}



func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, inState := userStates[chatID]; inState {
		cancelKb := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel")),
		)
		msg.ReplyMarkup = cancelKb
	}
	sendAndTrack(bot, msg)
}

func replyError(bot *tgbotapi.BotAPI, chatID int64, text string) {
	sendMessage(bot, chatID, "❌ "+text)
}

func cancelOperation(bot *tgbotapi.BotAPI, chatID int64, userID int64, config *BotConfig) {
	resetState(userID)
	showMainMenu(bot, chatID, config, userID)
}

func sendAndTrack(bot *tgbotapi.BotAPI, msg tgbotapi.MessageConfig) {
	deleteLastMessage(bot, msg.ChatID)
	sentMsg, err := bot.Send(msg)
	if err == nil {
		lastMessageIDs[msg.ChatID] = sentMsg.MessageID
	}
}

func deleteLastMessage(bot *tgbotapi.BotAPI, chatID int64) {
	if msgID, ok := lastMessageIDs[chatID]; ok {
		deleteMsg := tgbotapi.NewDeleteMessage(chatID, msgID)
		bot.Request(deleteMsg)
		delete(lastMessageIDs, chatID)
	}
}

func resetState(userID int64) {
	delete(userStates, userID)
	// Don't delete tempUserData immediately if pending payment, but here we do for cancel
}

func validatePassword(bot *tgbotapi.BotAPI, chatID int64, text string) bool {
	if len(text) < 3 || len(text) > 20 {
		sendMessage(bot, chatID, "❌ Password harus 3-20 karakter. Coba lagi:")
		return false
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(text) {
		sendMessage(bot, chatID, "❌ Password hanya boleh huruf, angka, - dan _. Coba lagi:")
		return false
	}
	return true
}

func validateNumber(bot *tgbotapi.BotAPI, chatID int64, text string, min, max int, fieldName string) (int, bool) {
	val, err := strconv.Atoi(text)
	if err != nil || val < min || val > max {
		sendMessage(bot, chatID, fmt.Sprintf("❌ %s harus angka positif (%d-%d). Coba lagi:", fieldName, min, max))
		return 0, false
	}
	return val, true
}

func systemInfo(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	res, err := apiCall("GET", "/info", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		ipInfo, _ := getIpInfo()

		msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n    INFO ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\nDomain         : %s\nIP Public      : %s\nPort           : %s\nService        : %s\nCITY           : %s\nISP            : %s\n━━━━━━━━━━━━━━━━━━━━━\n```",
			config.Domain, data["public_ip"], data["port"], data["service"], ipInfo.City, ipInfo.Isp)

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		deleteLastMessage(bot, chatID)
		bot.Send(reply)
		showMainMenu(bot, chatID, config, config.AdminID)
	} else {
		replyError(bot, chatID, "Gagal mengambil info.")
	}
}

func showBackupRestoreMenu(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "🛠️ *Admin Panel*\nSilakan pilih menu:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Backup Data", "menu_backup_action"),
			tgbotapi.NewInlineKeyboardButtonData("⬆️ Restore Data", "menu_restore_action"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Buat Akun Gratis", "menu_admin_create_free"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🧾 Manage Users", "menu_admin_manage"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Kembali", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

func showAdminManageMenu(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "⚙️ *Admin - Manage Users*\nPilih aksi:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Tambah Saldo", "admin_add_balance"),
			tgbotapi.NewInlineKeyboardButtonData("➖ Kurangi Saldo", "admin_remove_balance"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⛔ Ban User", "admin_ban"),
			tgbotapi.NewInlineKeyboardButtonData("✅ Unban User", "admin_unban"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📈 Lihat Aktivitas", "admin_view_activity"),
			tgbotapi.NewInlineKeyboardButtonData("📨 Mode Forward", "admin_forward_mode"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Kembali", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

func performBackup(bot *tgbotapi.BotAPI, chatID int64) {
	sendMessage(bot, chatID, "⏳ Sedang membuat backup...")

	// Files to backup
	files := []string{
		"/etc/zivpn/config.json",
		"/etc/zivpn/users.json",
		"/etc/zivpn/domain",
	}

	buf := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buf)

	for _, file := range files {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			continue
		}

		f, err := os.Open(file)
		if err != nil {
			continue
		}
		defer f.Close()

		w, err := zipWriter.Create(filepath.Base(file))
		if err != nil {
			continue
		}

		if _, err := io.Copy(w, f); err != nil {
			continue
		}
	}

	zipWriter.Close()

	fileName := fmt.Sprintf("zivpn-backup-%s.zip", time.Now().Format("20060102-150405"))
	
	// Create a temporary file for the upload
	tmpFile := "/tmp/" + fileName
	if err := ioutil.WriteFile(tmpFile, buf.Bytes(), 0644); err != nil {
		replyError(bot, chatID, "Gagal membuat file backup.")
		return
	}
	defer os.Remove(tmpFile)

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(tmpFile))
	doc.Caption = "✅ Backup Data ZiVPN"
	
	deleteLastMessage(bot, chatID)
	bot.Send(doc)
}

func startRestore(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "waiting_restore_file"
	sendMessage(bot, chatID, "⬆️ *Restore Data*\n\nSilakan kirim file ZIP backup Anda sekarang.\n\n⚠️ PERINGATAN: Data saat ini akan ditimpa!")
}

func processRestoreFile(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	chatID := msg.Chat.ID
	userID := msg.From.ID
	
	resetState(userID)
	sendMessage(bot, chatID, "⏳ Sedang memproses file...")

	// Download file
	fileID := msg.Document.FileID
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		replyError(bot, chatID, "Gagal mengunduh file.")
		return
	}

	fileUrl := file.Link(config.BotToken)
	resp, err := http.Get(fileUrl)
	if err != nil {
		replyError(bot, chatID, "Gagal mengunduh file content.")
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		replyError(bot, chatID, "Gagal membaca file.")
		return
	}

	// Unzip
	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		replyError(bot, chatID, "File bukan format ZIP yang valid.")
		return
	}

	for _, f := range zipReader.File {
		// Security check: only allow specific files
		validFiles := map[string]bool{
			"config.json": true,
			"users.json": true,
			"bot-config.json": true,
			"domain": true,
			"apikey": true,
		}
		
		if !validFiles[f.Name] {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}
		defer rc.Close()

		dstPath := filepath.Join("/etc/zivpn", f.Name)
		dst, err := os.Create(dstPath)
		if err != nil {
			continue
		}
		defer dst.Close()

		io.Copy(dst, rc)
	}

	// Restart Services
	exec.Command("systemctl", "restart", "zivpn").Run()
	exec.Command("systemctl", "restart", "zivpn-api").Run()
	
	msgSuccess := tgbotapi.NewMessage(chatID, "✅ Restore Berhasil!\nService ZiVPN, API, dan Bot telah direstart.")
	bot.Send(msgSuccess)

	// Restart Bot with delay to allow message sending
	go func() {
		time.Sleep(2 * time.Second)
		exec.Command("systemctl", "restart", "zivpn-bot").Run()
	}()

	showMainMenu(bot, chatID, config, config.AdminID)
}

func loadConfig() (BotConfig, error) {
	var config BotConfig
	file, err := ioutil.ReadFile(BotConfigFile)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(file, &config)

	if config.Domain == "" {
		if domainBytes, err := ioutil.ReadFile(DomainFile); err == nil {
			config.Domain = strings.TrimSpace(string(domainBytes))
		}
	}

	return config, err
}

func apiCall(method, endpoint string, payload interface{}) (map[string]interface{}, error) {
	var reqBody []byte
	var err error

	if payload != nil {
		reqBody, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}

	client := &http.Client{}
	req, err := http.NewRequest(method, ApiUrl+endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", ApiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	return result, nil
}

func getIpInfo() (IpInfo, error) {
	resp, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return IpInfo{}, err
	}
	defer resp.Body.Close()

	var info IpInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return IpInfo{}, err
	}
	return info, nil
}
