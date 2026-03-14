package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"trackalph.app/internal/config"
	"trackalph.app/internal/metrics"
	"trackalph.app/internal/models"
	"trackalph.app/internal/store"
	"trackalph.app/internal/stream"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

const (
	groupName    = "notifiers"
	consumerName = "notifier-1"
	metricsAddr  = ":2114"

	callbackWatch   = "watch:"
	callbackWebhook = "webhook:"
)

var (
	botCommandsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "trackalph_bot_commands_total",
		Help: "Total bot commands received.",
	}, []string{"command"})
	botCallbacksTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_bot_callbacks_total",
		Help: "Total callback queries received.",
	})
	botNotificationsConsumedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_bot_notifications_consumed_total",
		Help: "Total notification messages consumed by bot service.",
	})
	botNotificationsSentTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_bot_notifications_sent_total",
		Help: "Total Telegram notifications successfully sent.",
	})
	botNotificationErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_bot_notification_errors_total",
		Help: "Total Telegram notification send errors.",
	})
	addressesTrackedGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "trackalph_addresses_tracked",
		Help: "Current number of unique tracked addresses.",
	})
	addressesTrackedTotalGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "trackalph_addresses_tracked_total",
		Help: "Current number of unique tracked addresses (all channels).",
	})
	addressesTrackedTelegramGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "trackalph_addresses_tracked_telegram",
		Help: "Current number of unique addresses tracked by at least one Telegram subscriber.",
	})
	addressesTrackedWebhookGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "trackalph_addresses_tracked_webhook",
		Help: "Current number of unique addresses tracked by at least one webhook subscriber.",
	})
	botMessagesSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "trackalph_bot_messages_sent_total",
		Help: "Total Telegram messages send attempts by status.",
	}, []string{"status"})

	webhookSelectionSeq int64
	webhookSelectionMu  sync.Mutex
	webhookSelections   = make(map[string]webhookSelection)
)

type webhookSelection struct {
	Address   string
	URL       string
	CreatedAt time.Time
}

func main() {
	cfg := config.Load()
	metrics.StartServer(metricsAddr)

	if cfg.TelegramToken == "" {
		log.Fatal("TELEGRAM_TOKEN is required")
	}

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Fatalf("Invalid REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Redis connection failed: %v", err)
	}

	bot, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		log.Fatalf("Failed to create Telegram bot: %v", err)
	}
	log.Printf("Telegram bot authorized as @%s", bot.Self.UserName)

	st := store.New(rdb)
	str := stream.New(rdb)

	if err := str.CreateGroup(context.Background(), stream.NotificationsStream, groupName); err != nil {
		log.Fatalf("Failed to create consumer group: %v", err)
	}

	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		handleUpdates(appCtx, bot, st)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		reportAddressMetrics(appCtx, st)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		consumeNotifications(appCtx, bot, str)
	}()

	log.Println("Bot is running")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down bot...")
	cancel()
	wg.Wait()
	log.Println("Bot stopped")
}

// --- Update handler (commands + callback queries) ---

func handleUpdates(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store) {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			bot.StopReceivingUpdates()
			return
		case update := <-updates:
			if update.CallbackQuery != nil {
				go handleCallback(ctx, bot, st, update.CallbackQuery)
				continue
			}
			if update.Message != nil && update.Message.IsCommand() {
				go processCommand(ctx, bot, st, update.Message)
			}
		}
	}
}

func processCommand(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	botCommandsTotal.WithLabelValues(msg.Command()).Inc()

	switch msg.Command() {
	case "start":
		sendMessage(bot, chatID, welcomeMessage())
	case "help":
		sendMessage(bot, chatID, helpMessage())
	case "watch", "add":
		handleWatch(bot, chatID, msg.CommandArguments())
	case "unwatch", "remove":
		handleUnwatch(ctx, bot, st, chatID, msg.CommandArguments())
	case "list":
		handleList(ctx, bot, st, chatID)
	case "webhook":
		handleWebhookCmd(ctx, bot, st, chatID, msg.CommandArguments())
	case "rmwebhook":
		handleRmWebhook(ctx, bot, st, chatID, msg.CommandArguments())
	default:
		sendMessage(bot, chatID, "Unknown command. Use /help for available commands.")
	}
}

// --- /watch: validate address then show filter buttons ---

func handleWatch(bot *tgbotapi.BotAPI, chatID int64, args string) {
	raw := strings.TrimSpace(args)
	if raw == "" {
		sendMessage(bot, chatID, "Usage: <code>/watch &lt;address&gt; [label]</code>")
		return
	}
	parts := strings.Fields(raw)
	address := parts[0]
	address = normalizeAddress(address)
	label := ""
	if len(parts) > 1 {
		label = sanitizeLabel(strings.TrimSpace(strings.TrimPrefix(raw, address)))
	}

	switch detectAddressType(address) {
	case "alephium":
		// supported today
	case "ethereum":
		// supported today
	default:
		sendMessage(bot, chatID, "Invalid address. Please provide a valid Alephium or Ethereum address.")
		return
	}

	text := fmt.Sprintf("Choose notification filter for:\n<code>%s</code>", address)
	if label != "" {
		text += fmt.Sprintf("\nLabel: <code>%s</code>", html.EscapeString(label))
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML

	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("↕️ All", callbackWatch+store.FilterAll),
			tgbotapi.NewInlineKeyboardButtonData("📥 Incoming", callbackWatch+store.FilterIn),
			tgbotapi.NewInlineKeyboardButtonData("📤 Outgoing", callbackWatch+store.FilterOut),
		),
	)

	if _, err := bot.Send(msg); err != nil {
		log.Printf("Error sending filter menu to %d: %v", chatID, err)
	}
}

// --- /webhook: register a webhook URL for an address ---

func handleWebhookCmd(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, chatID int64, args string) {
	parts := strings.Fields(args)

	if len(parts) == 0 {
		handleWebhookList(ctx, bot, st, chatID)
		return
	}

	if len(parts) < 2 {
		sendMessage(bot, chatID, "Usage: <code>/webhook &lt;address&gt; &lt;url&gt;</code>\nOr <code>/webhook</code> to list all webhooks.")
		return
	}

	address := parts[0]
	address = normalizeAddress(address)
	webhookURL := parts[1]

	switch detectAddressType(address) {
	case "alephium":
		// supported today
	case "ethereum":
		// supported today
	default:
		sendMessage(bot, chatID, "Invalid address.")
		return
	}

	parsed, err := url.Parse(webhookURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		sendMessage(bot, chatID, "Invalid URL. Must start with <code>http://</code> or <code>https://</code>.")
		return
	}

	token := putWebhookSelection(address, webhookURL)
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf(
		"Choose webhook filter for:\n<code>%s</code>\n\nURL: %s", address, webhookURL))
	msg.ParseMode = tgbotapi.ModeHTML

	cbData := func(filter string) string { return callbackWebhook + filter + ":" + token }

	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("↕️ All", cbData(store.FilterAll)),
			tgbotapi.NewInlineKeyboardButtonData("📥 Incoming", cbData(store.FilterIn)),
			tgbotapi.NewInlineKeyboardButtonData("📤 Outgoing", cbData(store.FilterOut)),
		),
	)

	if _, err := bot.Send(msg); err != nil {
		log.Printf("Error sending webhook filter menu to %d: %v", chatID, err)
	}
}

func handleWebhookList(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, chatID int64) {
	hooks, err := st.GetWebhooksForChatID(ctx, chatID)
	if err != nil {
		log.Printf("Error listing webhooks: %v", err)
		sendMessage(bot, chatID, "Error retrieving webhooks. Please try again.")
		return
	}

	if len(hooks) == 0 {
		sendMessage(bot, chatID, "No webhooks configured.\nUse <code>/webhook &lt;address&gt; &lt;url&gt;</code> to add one.")
		return
	}

	filterIcon := map[string]string{
		store.FilterAll: "↕️",
		store.FilterIn:  "📥",
		store.FilterOut: "📤",
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<b>Webhooks (%d):</b>\n\n", len(hooks)))
	for i, h := range hooks {
		icon := filterIcon[h.Filter]
		if icon == "" {
			icon = "↕️"
		}
		sb.WriteString(fmt.Sprintf("%d. %s <code>%s</code>\n   → %s\n", i+1, icon, h.Address, h.URL))
	}
	sb.WriteString("\nUse <code>/rmwebhook &lt;address&gt;</code> to remove.")
	sendMessage(bot, chatID, sb.String())
}

// --- /rmwebhook ---

func handleRmWebhook(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, chatID int64, args string) {
	address := strings.TrimSpace(args)
	address = normalizeAddress(address)
	if address == "" {
		sendMessage(bot, chatID, "Usage: <code>/rmwebhook &lt;address&gt;</code>")
		return
	}

	if err := st.RemoveWebhook(ctx, chatID, address); err != nil {
		log.Printf("Error removing webhook: %v", err)
		sendMessage(bot, chatID, "Error removing webhook. Check the address and try again.")
		return
	}

	sendMessage(bot, chatID, fmt.Sprintf("Webhook removed for:\n<code>%s</code>", address))
}

// --- Callbacks ---

func handleCallback(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, cb *tgbotapi.CallbackQuery) {
	botCallbacksTotal.Inc()
	bot.Request(tgbotapi.NewCallback(cb.ID, ""))
	chatID := cb.Message.Chat.ID

	data := cb.Data
	switch {
	case strings.HasPrefix(data, callbackWatch):
		handleWatchCallback(ctx, bot, st, chatID, cb.Message.MessageID, strings.TrimPrefix(data, callbackWatch), cb.Message.Text)
	case strings.HasPrefix(data, callbackWebhook):
		handleWebhookCallback(ctx, bot, st, chatID, cb.Message.MessageID, strings.TrimPrefix(data, callbackWebhook))
	}
}

func handleWatchCallback(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, chatID int64, msgID int, filter string, sourceText string) {
	address, label := extractWatchSelection(sourceText)
	address = normalizeAddress(address)
	if address == "" {
		editMessageText(bot, chatID, msgID, "Could not read address from selection message. Please run /watch again.")
		return
	}
	if filter != store.FilterAll && filter != store.FilterIn && filter != store.FilterOut {
		return
	}

	if err := st.AddSubscription(ctx, chatID, address, filter, label); err != nil {
		log.Printf("Error adding subscription: %v", err)
		editMessageText(bot, chatID, msgID, "Error saving subscription. Please try again.")
		return
	}

	confirm := fmt.Sprintf("✅ Now watching address:\n<code>%s</code>\nFilter: <b>%s</b>", maskAddress(address), filterLabel(filter))
	if label != "" {
		confirm += fmt.Sprintf("\nLabel: <code>%s</code>", html.EscapeString(label))
	}
	editMessageText(bot, chatID, msgID, confirm)
}

func extractWatchSelection(text string) (string, string) {
	// Prompt format:
	// "Choose notification filter for:\n<address>\nLabel: <label>"
	lines := strings.Split(strings.TrimSpace(text), "\n")
	var address, label string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Choose notification filter for:") {
			continue
		}
		if strings.HasPrefix(line, "Label: ") {
			label = strings.TrimSpace(strings.TrimPrefix(line, "Label: "))
			continue
		}
		if address == "" {
			address = line
		}
	}
	return normalizeAddress(address), sanitizeLabel(label)
}

func maskAddress(addr string) string {
	if len(addr) <= 10 {
		return addr
	}
	return addr[:5] + "..." + addr[len(addr)-5:]
}

func sanitizeLabel(label string) string {
	label = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(label, "\n", " "), "\r", " "))
	if len(label) > 80 {
		label = label[:80]
	}
	return label
}

func putWebhookSelection(address, webhookURL string) string {
	id := atomic.AddInt64(&webhookSelectionSeq, 1)
	token := strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatInt(id, 36)

	webhookSelectionMu.Lock()
	defer webhookSelectionMu.Unlock()
	cleanupWebhookSelectionsLocked()
	webhookSelections[token] = webhookSelection{
		Address:   address,
		URL:       webhookURL,
		CreatedAt: time.Now(),
	}
	return token
}

func popWebhookSelection(token string) (webhookSelection, bool) {
	webhookSelectionMu.Lock()
	defer webhookSelectionMu.Unlock()
	cleanupWebhookSelectionsLocked()
	sel, ok := webhookSelections[token]
	if !ok {
		return webhookSelection{}, false
	}
	delete(webhookSelections, token)
	return sel, true
}

func cleanupWebhookSelectionsLocked() {
	cutoff := time.Now().Add(-15 * time.Minute)
	for k, v := range webhookSelections {
		if v.CreatedAt.Before(cutoff) {
			delete(webhookSelections, k)
		}
	}
}

func handleWebhookCallback(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, chatID int64, msgID int, data string) {
	// Format: "<filter>:<token>"
	parts := strings.SplitN(data, ":", 2)
	if len(parts) != 2 {
		return
	}
	filter := parts[0]
	token := parts[1]
	sel, ok := popWebhookSelection(token)
	if !ok {
		editMessageText(bot, chatID, msgID, "Webhook selection expired. Please run /webhook again.")
		return
	}
	address := sel.Address
	webhookURL := sel.URL

	if filter != store.FilterAll && filter != store.FilterIn && filter != store.FilterOut {
		return
	}

	if err := st.AddWebhook(ctx, chatID, address, webhookURL, filter); err != nil {
		log.Printf("Error adding webhook: %v", err)
		editMessageText(bot, chatID, msgID, "Error saving webhook. Please try again.")
		return
	}

	editMessageText(bot, chatID, msgID,
		fmt.Sprintf("🔗 Webhook registered:\n<code>%s</code>\nURL: %s\nFilter: <b>%s</b>", address, webhookURL, filterLabel(filter)))
}

// --- /unwatch ---

func handleUnwatch(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, chatID int64, args string) {
	address := strings.TrimSpace(args)
	address = normalizeAddress(address)
	if address == "" {
		sendMessage(bot, chatID, "Usage: <code>/unwatch &lt;address&gt;</code>")
		return
	}

	if err := st.RemoveSubscription(ctx, chatID, address); err != nil {
		log.Printf("Error removing subscription: %v", err)
		sendMessage(bot, chatID, "Error removing subscription. Please try again.")
		return
	}

	sendMessage(bot, chatID, fmt.Sprintf("Stopped watching address:\n<code>%s</code>", address))
}

// --- /list ---

func handleList(ctx context.Context, bot *tgbotapi.BotAPI, st *store.Store, chatID int64) {
	subs, err := st.GetSubscriptionsForChatID(ctx, chatID)
	if err != nil {
		log.Printf("Error listing addresses: %v", err)
		sendMessage(bot, chatID, "Error retrieving your addresses. Please try again.")
		return
	}

	hooks, _ := st.GetWebhooksForChatID(ctx, chatID)

	if len(subs) == 0 && len(hooks) == 0 {
		sendMessage(bot, chatID, "You are not watching any addresses.\nUse /watch &lt;address&gt; to start.")
		return
	}

	filterIcon := map[string]string{
		store.FilterAll: "↕️",
		store.FilterIn:  "📥",
		store.FilterOut: "📤",
	}

	var sb strings.Builder

	if len(subs) > 0 {
		sb.WriteString(fmt.Sprintf("<b>Telegram notifications (%d):</b>\n\n", len(subs)))
		for i, sub := range subs {
			icon := filterIcon[sub.Filter]
			if icon == "" {
				icon = "↕️"
			}
			sb.WriteString(fmt.Sprintf("%d. %s <code>%s</code>", i+1, icon, sub.Address))
			if sub.Label != "" {
				sb.WriteString(fmt.Sprintf(" — <i>%s</i>", html.EscapeString(sub.Label)))
			}
			sb.WriteString("\n")
		}
	}

	if len(hooks) > 0 {
		if len(subs) > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(fmt.Sprintf("<b>Webhooks (%d):</b>\n\n", len(hooks)))
		for i, h := range hooks {
			icon := filterIcon[h.Filter]
			if icon == "" {
				icon = "↕️"
			}
			sb.WriteString(fmt.Sprintf("%d. %s <code>%s</code>\n   → %s\n", i+1, icon, h.Address, h.URL))
		}
	}

	sb.WriteString("\n/unwatch &lt;address&gt; - remove Telegram\n/rmwebhook &lt;address&gt; - remove webhook")
	sendMessage(bot, chatID, sb.String())
}

// --- Message helpers ---

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.DisableWebPagePreview = true
	if _, err := bot.Send(msg); err != nil {
		botMessagesSentTotal.WithLabelValues("error").Inc()
		log.Printf("Error sending message to %d: %v", chatID, err)
	} else {
		botMessagesSentTotal.WithLabelValues("ok").Inc()
	}
}

func sendNotificationMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	msg.DisableWebPagePreview = true
	if _, err := bot.Send(msg); err != nil {
		botMessagesSentTotal.WithLabelValues("error").Inc()
		botNotificationErrorsTotal.Inc()
		log.Printf("Error sending notification to %d: %v", chatID, err)
	} else {
		botMessagesSentTotal.WithLabelValues("ok").Inc()
		botNotificationsSentTotal.Inc()
	}
}

func reportAddressMetrics(ctx context.Context, st *store.Store) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()

	for {
		total, telegram, webhook := st.CountWatchedAddressesByChannel(ctx)
		addressesTrackedGauge.Set(float64(total))
		addressesTrackedTotalGauge.Set(float64(total))
		addressesTrackedTelegramGauge.Set(float64(telegram))
		addressesTrackedWebhookGauge.Set(float64(webhook))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func editMessageText(bot *tgbotapi.BotAPI, chatID int64, messageID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = tgbotapi.ModeHTML
	edit.DisableWebPagePreview = true
	if _, err := bot.Send(edit); err != nil {
		log.Printf("Error editing message: %v", err)
	}
}

func filterLabel(f string) string {
	switch f {
	case store.FilterIn:
		return "incoming only"
	case store.FilterOut:
		return "outgoing only"
	default:
		return "all transactions"
	}
}

// --- Notification consumer (telegram only) ---

func consumeNotifications(ctx context.Context, bot *tgbotapi.BotAPI, str *stream.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := str.Consume(ctx, stream.NotificationsStream, groupName, consumerName)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Error consuming notifications: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, msg := range msgs {
			botNotificationsConsumedTotal.Inc()
			data, ok := msg.Values["data"].(string)
			if !ok {
				str.Ack(ctx, stream.NotificationsStream, groupName, msg.ID)
				continue
			}

			var notif models.Notification
			if err := json.Unmarshal([]byte(data), &notif); err != nil {
				log.Printf("Error unmarshaling notification: %v", err)
				str.Ack(ctx, stream.NotificationsStream, groupName, msg.ID)
				continue
			}

			if notif.Channel == models.ChannelTelegram {
				sendNotificationMessage(bot, notif.ChatID, notif.Message)
			}
			str.Ack(ctx, stream.NotificationsStream, groupName, msg.ID)
		}
	}
}

// --- Helpers ---

func welcomeMessage() string {
	return `<b>Welcome to TrackAlph!</b>

I notify you when transactions happen on your Alephium or Ethereum addresses.

<b>Commands:</b>
/watch &lt;address&gt; [label] - Watch via Telegram
/webhook &lt;address&gt; &lt;url&gt; - Watch via webhook
/unwatch &lt;address&gt; - Stop Telegram notifications
/rmwebhook &lt;address&gt; - Remove webhook
/list - Show all subscriptions
/help - Show this help

Get started by sending:
<code>/watch YOUR_ALEPHIUM_ADDRESS</code>

Developed by <a href="https://notrustverify.ch">notrustverify.ch</a>`
}

func helpMessage() string {
	return `<b>TrackAlph Bot - Commands</b>

<b>Telegram notifications:</b>
/watch &lt;address&gt; [label] - Watch an address (choose filter via buttons)
/unwatch &lt;address&gt; - Stop watching an address

<b>Other:</b>
/webhook &lt;address&gt; &lt;url&gt; - Register a webhook
/webhook - List your webhooks
/rmwebhook &lt;address&gt; - Remove a webhook
/list - List all subscriptions
/help - Show this help message

<b>Label examples:</b>
<code>/watch 1Abc... main wallet</code>
<code>/watch 1Abc... treasury</code>

To change a filter, just re-register with the same address.

Developed by <a href="https://notrustverify.ch">notrustverify.ch</a>`
}

func isValidAlephiumAddress(addr string) bool {
	// Keep lightweight validation to avoid rejecting future or uncommon
	// address/script formats while still catching obvious mistakes.
	if len(addr) < 40 || len(addr) > 2048 {
		return false
	}

	const base58Chars = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	for _, c := range addr {
		if !strings.ContainsRune(base58Chars, c) {
			return false
		}
	}

	return true
}

func normalizeAddress(addr string) string {
	addr = strings.TrimSpace(addr)
	if isEthereumAddress(addr) {
		return strings.ToLower(addr)
	}
	return addr
}

func detectAddressType(addr string) string {
	if isEthereumAddress(addr) {
		return "ethereum"
	}
	if isValidAlephiumAddress(addr) {
		return "alephium"
	}
	return ""
}

func isEthereumAddress(addr string) bool {
	if len(addr) != 42 {
		return false
	}
	if !strings.HasPrefix(addr, "0x") {
		return false
	}
	for _, c := range addr[2:] {
		if !isHexDigit(c) {
			return false
		}
	}
	return true
}

func isHexDigit(c rune) bool {
	return (c >= '0' && c <= '9') ||
		(c >= 'a' && c <= 'f') ||
		(c >= 'A' && c <= 'F')
}
