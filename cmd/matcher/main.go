package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"trackalph.app/internal/config"
	"trackalph.app/internal/explorer"
	"trackalph.app/internal/metrics"
	"trackalph.app/internal/models"
	"trackalph.app/internal/store"
	"trackalph.app/internal/stream"
	"trackalph.app/internal/tokens"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

const (
	groupName    = "matchers"
	consumerName = "matcher-1"
	numWorkers   = 10
	metricsAddr  = ":2113"
)

var (
	matcherBlocksConsumedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_matcher_blocks_consumed_total",
		Help: "Total blocks consumed from Redis stream.",
	})
	matcherTxJobsEnqueuedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_matcher_tx_jobs_enqueued_total",
		Help: "Total transaction jobs enqueued for processing.",
	})
	matcherTxProcessedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_matcher_tx_processed_total",
		Help: "Total transactions fetched and processed.",
	})
	matcherExplorerFetchErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_matcher_explorer_fetch_errors_total",
		Help: "Total explorer fetch errors.",
	})
	matcherNotificationsPublishedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_matcher_notifications_published_total",
		Help: "Total notifications published to Redis.",
	})
	matcherNotificationPublishErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "trackalph_matcher_notification_publish_errors_total",
		Help: "Total notification publish errors.",
	})
	matcherTxProcessDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "trackalph_matcher_tx_process_duration_seconds",
		Help:    "Duration of processTx execution.",
		Buckets: prometheus.DefBuckets,
	})
)

func main() {
	cfg := config.Load()
	metrics.StartServer(metricsAddr)

	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Fatalf("Invalid REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("Redis connection failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := store.New(rdb)
	str := stream.New(rdb)
	exp := explorer.New(cfg.ExplorerAPI)
	tok := tokens.NewCache(cfg.TokenListURL)

	if err := tok.Start(ctx); err != nil {
		log.Fatalf("Failed to start token cache: %v", err)
	}

	if err := str.CreateGroup(ctx, stream.BlocksStream, groupName); err != nil {
		log.Fatalf("Failed to create consumer group: %v", err)
	}

	m := &matcher{
		cfg:      cfg,
		store:    st,
		stream:   str,
		explorer: exp,
		tokens:   tok,
		txCh:     make(chan txJob, 500),
	}

	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.txWorker(ctx)
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		m.consumeBlocks(ctx)
	}()

	log.Println("Matcher is running")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down matcher...")
	cancel()
	wg.Wait()
	log.Println("Matcher stopped")
}

type txJob struct {
	ref   models.TxRef
	block *models.WsBlockNotify
}

type matcher struct {
	cfg      config.Config
	store    *store.Store
	stream   *stream.Client
	explorer *explorer.Client
	tokens   *tokens.Cache
	txCh     chan txJob
}

func (m *matcher) consumeBlocks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := m.stream.Consume(ctx, stream.BlocksStream, groupName, consumerName)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Error consuming blocks: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, msg := range msgs {
			matcherBlocksConsumedTotal.Inc()
			data, ok := msg.Values["data"].(string)
			if !ok {
				m.stream.Ack(ctx, stream.BlocksStream, groupName, msg.ID)
				continue
			}

			var block models.WsBlockNotify
			if err := json.Unmarshal([]byte(data), &block); err != nil {
				log.Printf("Error unmarshaling block: %v", err)
				m.stream.Ack(ctx, stream.BlocksStream, groupName, msg.ID)
				continue
			}

			m.processBlock(ctx, &block)
			m.stream.Ack(ctx, stream.BlocksStream, groupName, msg.ID)
		}
	}
}

func (m *matcher) processBlock(ctx context.Context, block *models.WsBlockNotify) {
	for _, tx := range block.Params.Transactions {
		if len(tx.Unsigned.Inputs) == 0 {
			continue
		}

		if !m.txMatchesWatched(ctx, tx) {
			continue
		}

		ref := models.TxRef{
			ID:        tx.Unsigned.TxID,
			GroupFrom: block.Params.ChainFrom,
			GroupTo:   block.Params.ChainTo,
			Height:    block.Params.Height,
		}

		select {
		case m.txCh <- txJob{ref: ref, block: block}:
			matcherTxJobsEnqueuedTotal.Inc()
		case <-ctx.Done():
			return
		}
	}
}

func (m *matcher) txMatchesWatched(ctx context.Context, tx models.BlockTx) bool {
	for _, out := range tx.Unsigned.FixedOutputs {
		if m.store.IsWatched(ctx, out.Address) {
			return true
		}
	}
	for _, out := range tx.GeneratedOutputs {
		if out.Address != "" && m.store.IsWatched(ctx, out.Address) {
			return true
		}
	}
	return false
}

func (m *matcher) txWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-m.txCh:
			m.processTx(ctx, job.ref)
		}
	}
}

func (m *matcher) processTx(ctx context.Context, ref models.TxRef) {
	start := time.Now()
	defer matcherTxProcessDuration.Observe(time.Since(start).Seconds())

	tx, err := m.explorer.FetchTransaction(ctx, ref.ID)
	if err != nil {
		matcherExplorerFetchErrorsTotal.Inc()
		if ctx.Err() == nil {
			log.Printf("Error fetching tx %s: %v", ref.ID, err)
		}
		return
	}
	matcherTxProcessedTotal.Inc()

	inputAddrs := make(map[string]struct{})
	for _, inp := range tx.Inputs {
		if inp.Address != "" {
			inputAddrs[inp.Address] = struct{}{}
		}
	}

	isContract := false
	for _, out := range tx.Outputs {
		if strings.EqualFold(out.Type, "contractoutput") {
			isContract = true
			break
		}
	}

	involved := make(map[string]struct{})
	for addr := range inputAddrs {
		if m.store.IsWatched(ctx, addr) {
			involved[addr] = struct{}{}
		}
	}
	for _, out := range tx.Outputs {
		if out.Address != "" && !strings.EqualFold(out.Type, "contractoutput") && m.store.IsWatched(ctx, out.Address) {
			involved[out.Address] = struct{}{}
		}
	}

	for addr := range involved {
		flow := m.calculateFlow(addr, tx)
		_, isSender := inputAddrs[addr]

		hasFlow := flow.sentAlph > 0.001 || flow.receivedAlph > 0.001 ||
			len(flow.sentTokens) > 0 || len(flow.receivedTokens) > 0

		if !isContract && !hasFlow {
			continue
		}

		// Determine direction for filter matching
		hasSent := flow.sentAlph > 0 || len(flow.sentTokens) > 0
		hasReceived := flow.receivedAlph > 0 || len(flow.receivedTokens) > 0

		var msg string
		if isContract {
			msg = m.formatContractNotification(addr, flow, tx, ref)
		} else {
			msg = m.formatTransferNotification(addr, flow, isSender, tx, ref)
		}

		for _, sub := range m.store.GetSubscribersForAddress(ctx, addr) {
			if sub.Channel != models.ChannelTelegram {
				// Webhook channel is temporarily disabled.
				continue
			}
			if !matchesFilter(sub.Filter, isSender, hasSent, hasReceived, isContract) {
				continue
			}
			notif := models.Notification{
				Channel: sub.Channel,
				ChatID:  sub.ChatID,
				URL:     sub.URL,
				Message: msg,
			}
			data, _ := json.Marshal(notif)
			if err := m.stream.Publish(ctx, stream.NotificationsStream, data); err != nil {
				matcherNotificationPublishErrorsTotal.Inc()
				log.Printf("Error publishing notification: %v", err)
			} else {
				matcherNotificationsPublishedTotal.Inc()
			}
		}
	}
}

func matchesFilter(filter string, isSender, hasSent, hasReceived, isContract bool) bool {
	switch filter {
	case store.FilterIn:
		if isContract {
			return hasReceived
		}
		return !isSender
	case store.FilterOut:
		if isContract {
			return hasSent
		}
		return isSender
	default:
		return true
	}
}

// --- Flow calculation ---

type txFlow struct {
	sentAlph       float64
	receivedAlph   float64
	sentTokens     map[string]float64
	receivedTokens map[string]float64
	tokenSymbols   map[string]string
	gasAlph        float64
	contractAddrs  []string
}

func (m *matcher) calculateFlow(watchedAddr string, tx *models.Transaction) txFlow {
	flow := txFlow{
		sentTokens:     make(map[string]float64),
		receivedTokens: make(map[string]float64),
		tokenSymbols:   make(map[string]string),
	}

	isSender := false
	var inputAlph float64
	inputTokens := make(map[string]float64)

	for _, inp := range tx.Inputs {
		if inp.Address != watchedAddr {
			continue
		}
		isSender = true
		inputAlph += models.ParseFloat(inp.AttoAlphAmount) / models.AttoAlphDivisor
		for _, tok := range inp.Tokens {
			amt, sym := m.humanizeToken(tok)
			inputTokens[tok.ID] += amt
			flow.tokenSymbols[tok.ID] = sym
		}
	}

	var outputAlph float64
	outputTokens := make(map[string]float64)
	contractSeen := make(map[string]struct{})

	for _, out := range tx.Outputs {
		if strings.EqualFold(out.Type, "contractoutput") {
			if _, ok := contractSeen[out.Address]; !ok {
				contractSeen[out.Address] = struct{}{}
				flow.contractAddrs = append(flow.contractAddrs, out.Address)
			}
			continue
		}
		if out.Address != watchedAddr {
			continue
		}
		outputAlph += models.ParseFloat(out.AttoAlphAmount) / models.AttoAlphDivisor
		for _, tok := range out.Tokens {
			amt, sym := m.humanizeToken(tok)
			outputTokens[tok.ID] += amt
			flow.tokenSymbols[tok.ID] = sym
		}
	}

	if isSender {
		flow.gasAlph = (float64(tx.GasAmount) * models.ParseFloat(tx.GasPrice)) / models.AttoAlphDivisor
	}

	netAlph := outputAlph - inputAlph + flow.gasAlph
	if netAlph < -0.0001 {
		flow.sentAlph = -netAlph
	} else if netAlph > 0.0001 {
		flow.receivedAlph = netAlph
	}

	allTokenIDs := make(map[string]struct{})
	for id := range inputTokens {
		allTokenIDs[id] = struct{}{}
	}
	for id := range outputTokens {
		allTokenIDs[id] = struct{}{}
	}
	for id := range allTokenIDs {
		net := outputTokens[id] - inputTokens[id]
		if net < -0.0001 {
			flow.sentTokens[id] = -net
		} else if net > 0.0001 {
			flow.receivedTokens[id] = net
		}
	}

	return flow
}

func (m *matcher) humanizeToken(tok models.TokenTransfer) (float64, string) {
	amt, sym, found := m.tokens.HumanizeAmount(tok.ID, tok.Amount)
	if found {
		return amt, sym
	}
	return models.ParseFloat(tok.Amount), fmt.Sprintf("?(%s…)", truncateID(tok.ID))
}

// --- Notification formatting ---

func (m *matcher) formatTransferNotification(watchedAddr string, flow txFlow, isSender bool, tx *models.Transaction, ref models.TxRef) string {
	var b strings.Builder

	if isSender {
		fmt.Fprintf(&b, "📤 <b>Sent</b>\n\n")
		writeAmounts(&b, "💰", flow.sentAlph, flow.sentTokens, flow.tokenSymbols)

		fmt.Fprintf(&b, "\nFrom: <code>%s</code>\n", truncateAddress(watchedAddr))

		seen := make(map[string]struct{})
		var recipients []string
		for _, out := range tx.Outputs {
			if out.Address == watchedAddr || out.Address == "" || strings.EqualFold(out.Type, "contractoutput") {
				continue
			}
			if _, ok := seen[out.Address]; !ok {
				seen[out.Address] = struct{}{}
				recipients = append(recipients, out.Address)
			}
		}
		if len(recipients) == 1 {
			fmt.Fprintf(&b, "To: <code>%s</code>\n", truncateAddress(recipients[0]))
		} else if len(recipients) > 1 {
			fmt.Fprintf(&b, "To: %d addresses\n", len(recipients))
		}
	} else {
		fmt.Fprintf(&b, "📥 <b>Received</b>\n\n")
		writeAmounts(&b, "💰", flow.receivedAlph, flow.receivedTokens, flow.tokenSymbols)

		senderAddr := "unknown"
		if len(tx.Inputs) > 0 && tx.Inputs[0].Address != "" {
			senderAddr = tx.Inputs[0].Address
		}
		fmt.Fprintf(&b, "\nFrom: <code>%s</code>\n", truncateAddress(senderAddr))
		fmt.Fprintf(&b, "To: <code>%s</code>\n", truncateAddress(watchedAddr))
	}

	fmt.Fprintf(&b, "Chain: %d → %d\n", ref.GroupFrom, ref.GroupTo)
	fmt.Fprintf(&b, "\n<a href=\"%s/#/transactions/%s\">View on Explorer</a>", m.cfg.ExplorerURL, tx.Hash)
	return b.String()
}

func (m *matcher) formatContractNotification(watchedAddr string, flow txFlow, tx *models.Transaction, ref models.TxRef) string {
	var b strings.Builder

	if tx.ScriptExecutionOk {
		fmt.Fprintf(&b, "⚙️ <b>Contract Interaction</b>\n\n")
	} else {
		fmt.Fprintf(&b, "⚙️ <b>Contract Interaction</b> ❌\n\n")
	}

	hasSent := flow.sentAlph > 0 || len(flow.sentTokens) > 0
	hasReceived := flow.receivedAlph > 0 || len(flow.receivedTokens) > 0

	if hasSent {
		if flow.sentAlph > 0 {
			fmt.Fprintf(&b, "↗ Sent: <b>%s ALPH</b>\n", humanizeNumber(flow.sentAlph))
		}
		for id, amt := range flow.sentTokens {
			fmt.Fprintf(&b, "↗ Sent: <b>%s %s</b>\n", humanizeNumber(amt), flow.tokenSymbols[id])
		}
	}

	if hasReceived {
		if flow.receivedAlph > 0 {
			fmt.Fprintf(&b, "↙ Received: <b>%s ALPH</b>\n", humanizeNumber(flow.receivedAlph))
		}
		for id, amt := range flow.receivedTokens {
			fmt.Fprintf(&b, "↙ Received: <b>%s %s</b>\n", humanizeNumber(amt), flow.tokenSymbols[id])
		}
	}

	if !hasSent && !hasReceived && !tx.ScriptExecutionOk {
		fmt.Fprintf(&b, "Transaction reverted\n")
	}

	if flow.gasAlph > 0 {
		fmt.Fprintf(&b, "⛽ Gas: %s ALPH\n", humanizeNumber(flow.gasAlph))
	}

	fmt.Fprintf(&b, "\nAddress: <code>%s</code>\n", truncateAddress(watchedAddr))

	if len(flow.contractAddrs) == 1 {
		fmt.Fprintf(&b, "Contract: <code>%s</code>\n", truncateAddress(flow.contractAddrs[0]))
	} else if len(flow.contractAddrs) > 1 {
		fmt.Fprintf(&b, "Contracts: %d addresses\n", len(flow.contractAddrs))
	}

	fmt.Fprintf(&b, "Chain: %d → %d\n", ref.GroupFrom, ref.GroupTo)
	fmt.Fprintf(&b, "\n<a href=\"%s/#/transactions/%s\">View on Explorer</a>", m.cfg.ExplorerURL, tx.Hash)
	return b.String()
}

func writeAmounts(b *strings.Builder, emoji string, alph float64, toks map[string]float64, symbols map[string]string) {
	if alph > 0 {
		fmt.Fprintf(b, "%s <b>%s ALPH</b>\n", emoji, humanizeNumber(alph))
	}
	for id, amt := range toks {
		fmt.Fprintf(b, "🪙 <b>%s %s</b>\n", humanizeNumber(amt), symbols[id])
	}
}

func truncateAddress(addr string) string {
	if len(addr) <= 12 {
		return addr
	}
	return addr[:6] + "…" + addr[len(addr)-6:]
}

func truncateID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:8]
}

func humanizeNumber(n float64) string {
	abs := math.Abs(n)
	switch {
	case abs >= 1e6:
		return fmt.Sprintf("%.2fM", n/1e6)
	case abs >= 1e3:
		return fmt.Sprintf("%.2fK", n/1e3)
	case abs >= 1:
		return fmt.Sprintf("%.2f", n)
	default:
		return fmt.Sprintf("%.4f", n)
	}
}
