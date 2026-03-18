package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
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
	ethGroupName = "matchers-eth"
	ethConsumer  = "matcher-eth-1"
	numWorkers   = 10
	metricsAddr  = ":2113"
)

var ethHTTPClient = &http.Client{Timeout: 15 * time.Second}

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
	if cfg.EthRPC != "" {
		if err := str.CreateGroup(ctx, stream.EthBlocksStream, ethGroupName); err != nil {
			log.Fatalf("Failed to create ETH consumer group: %v", err)
		}
	}

	m := &matcher{
		cfg:      cfg,
		store:    st,
		stream:   str,
		explorer: exp,
		tokens:   tok,
		txCh:     make(chan txJob, 500),
		lastSeen: make(map[string]int),
		ethTokenMeta: make(map[string]ethTokenMetadata),
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

	if cfg.EthRPC != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.consumeEthBlocks(ctx)
		}()
	}

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
	lastSeen map[string]int
	ethTokenMu sync.RWMutex
	ethTokenMeta map[string]ethTokenMetadata
}

type ethTokenMetadata struct {
	Symbol   string
	Decimals int
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

func (m *matcher) consumeEthBlocks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		msgs, err := m.stream.Consume(ctx, stream.EthBlocksStream, ethGroupName, ethConsumer)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("Error consuming ETH blocks: %v", err)
			time.Sleep(1 * time.Second)
			continue
		}

		for _, msg := range msgs {
			data, ok := msg.Values["data"].(string)
			if !ok {
				m.stream.Ack(ctx, stream.EthBlocksStream, ethGroupName, msg.ID)
				continue
			}
			var ref models.EthBlockRef
			if err := json.Unmarshal([]byte(data), &ref); err != nil {
				m.stream.Ack(ctx, stream.EthBlocksStream, ethGroupName, msg.ID)
				continue
			}
			m.processEthBlock(ctx, ref)
			m.stream.Ack(ctx, stream.EthBlocksStream, ethGroupName, msg.ID)
		}
	}
}

type ethRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type ethRPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type ethTx struct {
	Hash     string `json:"hash"`
	From     string `json:"from"`
	To       string `json:"to"`
	Value    string `json:"value"`
	Input    string `json:"input"`
	GasPrice string `json:"gasPrice"`
}

type ethBlock struct {
	Number       string  `json:"number"`
	Hash         string  `json:"hash"`
	Transactions []ethTx `json:"transactions"`
}

type ethReceipt struct {
	Status            string `json:"status"`
	GasUsed           string `json:"gasUsed"`
	EffectiveGasPrice string `json:"effectiveGasPrice"`
	Logs              []ethLog `json:"logs"`
}

type ethLog struct {
	Address string   `json:"address"`
	Topics  []string `json:"topics"`
	Data    string   `json:"data"`
}

type erc20Transfer struct {
	TokenAddr string
	From      string
	To        string
	Amount    *big.Int
	Symbol    string
	Decimals  int
}

const (
	erc20TransferTopic   = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	wethDepositTopic    = "0xe1fffcc4923d04b559f4d29a8bfc6cda04eb5b0d3c460751c2402c5c5cc9109c" // Deposit(address indexed dst, uint256 wad)
	wethWithdrawalTopic = "0x7fcf532c15f0a6db0bd6d0e038bea71d30d808c7d98cb3bf7268a95bf5081b65" // Withdrawal(address indexed src, uint256 wad)
)

type wethFlow struct {
	Addr   string
	Amount *big.Int
	IsSent bool // true=Deposit (sent ETH), false=Withdrawal (received ETH)
}

func (m *matcher) processEthBlock(ctx context.Context, ref models.EthBlockRef) {
	if m.cfg.EthRPC == "" || ref.Hash == "" {
		return
	}

	var block ethBlock
	if err := m.ethRPCCall(ctx, "eth_getBlockByHash", []any{ref.Hash, true}, &block); err != nil {
		log.Printf("Error fetching ETH block %s: %v", ref.Hash, err)
		return
	}

	blockNum := hexToInt64(block.Number)
	for _, tx := range block.Transactions {
		from := strings.ToLower(tx.From)
		to := strings.ToLower(tx.To)

		receipt := ethReceipt{}
		_ = m.ethRPCCall(ctx, "eth_getTransactionReceipt", []any{tx.Hash}, &receipt)
		tokenTransfers := m.extractERC20Transfers(ctx, receipt.Logs)
		wethFlows := extractWethFlows(receipt.Logs)

		involved := map[string]struct{}{}
		if from != "" && m.store.IsWatched(ctx, from) {
			involved[from] = struct{}{}
		}
		if to != "" && m.store.IsWatched(ctx, to) {
			involved[to] = struct{}{}
		}
		for _, tr := range tokenTransfers {
			if tr.From != "" && m.store.IsWatched(ctx, tr.From) {
				involved[tr.From] = struct{}{}
			}
			if tr.To != "" && m.store.IsWatched(ctx, tr.To) {
				involved[tr.To] = struct{}{}
			}
		}
		for _, wf := range wethFlows {
			if wf.Addr != "" && m.store.IsWatched(ctx, wf.Addr) {
				involved[wf.Addr] = struct{}{}
			}
		}
		if len(involved) == 0 {
			continue
		}

		gasPrice := receipt.EffectiveGasPrice
		if gasPrice == "" || gasPrice == "0x" || gasPrice == "0x0" {
			gasPrice = tx.GasPrice
		}
		gasWeiHex := mulHex(receipt.GasUsed, gasPrice)
		gasWei := hexToBigInt(gasWeiHex)
		success := receipt.Status == "" || receipt.Status == "0x1"
		explorerURL := strings.TrimRight(m.cfg.EthExplorerURL, "/") + "/tx/" + tx.Hash
		isContract := tx.Input != "0x" || len(tokenTransfers) > 0 || len(wethFlows) > 0

		for addr := range involved {
			ethSentWei := big.NewInt(0)
			ethReceivedWei := big.NewInt(0)
			if addr == from {
				ethSentWei = new(big.Int).Set(hexToBigInt(tx.Value))
			}
			if addr == to {
				ethReceivedWei = new(big.Int).Set(hexToBigInt(tx.Value))
			}
			for _, wf := range wethFlows {
				if wf.Addr != addr {
					continue
				}
				if wf.IsSent {
					ethSentWei = new(big.Int).Add(ethSentWei, wf.Amount)
				} else {
					ethReceivedWei = new(big.Int).Add(ethReceivedWei, wf.Amount)
				}
			}

			sentTokens := map[string]erc20Transfer{}
			receivedTokens := map[string]erc20Transfer{}
			for _, tr := range tokenTransfers {
				if tr.From == addr {
					acc := sentTokens[tr.TokenAddr]
					if acc.Amount == nil {
						acc = tr
						acc.Amount = big.NewInt(0)
					}
					acc.Amount = new(big.Int).Add(acc.Amount, tr.Amount)
					sentTokens[tr.TokenAddr] = acc
				}
				if tr.To == addr {
					acc := receivedTokens[tr.TokenAddr]
					if acc.Amount == nil {
						acc = tr
						acc.Amount = big.NewInt(0)
					}
					acc.Amount = new(big.Int).Add(acc.Amount, tr.Amount)
					receivedTokens[tr.TokenAddr] = acc
				}
			}

			hasSent := ethSentWei.Sign() > 0 || len(sentTokens) > 0
			hasReceived := ethReceivedWei.Sign() > 0 || len(receivedTokens) > 0
			if !hasSent && !hasReceived && !isContract {
				continue
			}

			isSender := addr == from
			msg := formatEthTransferNotification(
				addr, from, to, blockNum, success, explorerURL,
				ethSentWei, ethReceivedWei, sentTokens, receivedTokens, gasWei, isSender,
			)
			actionType, actionAmount, actionToken := deriveEthPrimaryAction(ethSentWei, ethReceivedWei, sentTokens, receivedTokens)
			eventDir := "in"
			if actionType == "sent" {
				eventDir = "out"
			}
			actionGas := 0.0
			if isSender {
				actionGas = weiBigIntToEthFloat(gasWei)
			}

			for _, sub := range m.store.GetSubscribersForAddress(ctx, addr) {
				if !matchesFilter(sub.Filter, isSender, hasSent, hasReceived, isContract) {
					continue
				}
				msgOut := msg
				if sub.Channel == models.ChannelTelegram {
					if label := m.store.GetAddressLabel(ctx, sub.ChatID, addr); label != "" {
						msgOut = fmt.Sprintf("🏷️ <b>%s</b>\n\n%s", html.EscapeString(label), msgOut)
					}
				}
				notif := models.Notification{
					Channel:      sub.Channel,
					ChatID:       sub.ChatID,
					URL:          sub.URL,
					Message:      msgOut,
					Event:        ethEventType(eventDir, isContract),
					ActionType:   actionType,
					ActionAmount: actionAmount,
					ActionToken:  actionToken,
					GasAmount:    actionGas,
					GasToken:     "ETH",
					Address:      addr,
					Contract:     to,
					ChainFrom:    0,
					ChainTo:      0,
					ExplorerURL:  explorerURL,
				}
				data, _ := json.Marshal(notif)
				if err := m.stream.Publish(ctx, stream.NotificationsStream, data); err != nil {
					log.Printf("Error publishing ETH notification: %v", err)
				}
			}
		}
	}
}

func (m *matcher) extractERC20Transfers(ctx context.Context, logs []ethLog) []erc20Transfer {
	out := make([]erc20Transfer, 0)
	for _, lg := range logs {
		if len(lg.Topics) < 3 || !strings.EqualFold(lg.Topics[0], erc20TransferTopic) {
			continue
		}
		from := topicToAddress(lg.Topics[1])
		to := topicToAddress(lg.Topics[2])
		amount := hexToBigInt(lg.Data)
		if amount.Sign() == 0 {
			continue
		}
		contract := strings.ToLower(lg.Address)
		meta := m.getEthTokenMetadata(ctx, contract)
		out = append(out, erc20Transfer{
			TokenAddr: contract,
			From:      from,
			To:        to,
			Amount:    amount,
			Symbol:    meta.Symbol,
			Decimals:  meta.Decimals,
		})
	}
	return out
}

func extractWethFlows(logs []ethLog) []wethFlow {
	out := make([]wethFlow, 0)
	for _, lg := range logs {
		if len(lg.Topics) < 2 {
			continue
		}
		amount := hexToBigInt(lg.Data)
		if amount.Sign() == 0 {
			continue
		}
		addr := topicToAddress(lg.Topics[1])
		if addr == "" {
			continue
		}
		switch strings.ToLower(lg.Topics[0]) {
		case wethDepositTopic:
			// Deposit(dst, wad): dst sent wad wei of ETH to WETH contract
			out = append(out, wethFlow{Addr: addr, Amount: amount, IsSent: true})
		case wethWithdrawalTopic:
			// Withdrawal(src, wad): src received wad wei of ETH from WETH contract
			out = append(out, wethFlow{Addr: addr, Amount: amount, IsSent: false})
		}
	}
	return out
}

func (m *matcher) getEthTokenMetadata(ctx context.Context, tokenAddr string) ethTokenMetadata {
	if tokenAddr == "" {
		return ethTokenMetadata{Symbol: "TOKEN", Decimals: 18}
	}
	tokenAddr = strings.ToLower(tokenAddr)
	m.ethTokenMu.RLock()
	if meta, ok := m.ethTokenMeta[tokenAddr]; ok {
		m.ethTokenMu.RUnlock()
		return meta
	}
	m.ethTokenMu.RUnlock()

	meta := ethTokenMetadata{Symbol: "TOKEN", Decimals: 18}
	if d, err := m.ethCall(ctx, tokenAddr, "0x313ce567"); err == nil {
		if dec := int(hexToBigInt(d).Int64()); dec >= 0 && dec <= 36 {
			meta.Decimals = dec
		}
	}
	if s, err := m.ethCall(ctx, tokenAddr, "0x95d89b41"); err == nil {
		if sym := decodeERC20Symbol(s); sym != "" {
			meta.Symbol = sym
		}
	}

	m.ethTokenMu.Lock()
	m.ethTokenMeta[tokenAddr] = meta
	m.ethTokenMu.Unlock()
	return meta
}

func (m *matcher) ethCall(ctx context.Context, to, data string) (string, error) {
	var out string
	err := m.ethRPCCall(ctx, "eth_call", []any{
		map[string]any{
			"to":   to,
			"data": data,
		},
		"latest",
	}, &out)
	return out, err
}

func deriveEthPrimaryAction(ethSent, ethReceived *big.Int, sentTokens, receivedTokens map[string]erc20Transfer) (string, float64, string) {
	if tr, ok := firstTokenTransfer(receivedTokens); ok {
		return "received", tokenAmountToFloat(tr.Amount, tr.Decimals), tr.Symbol
	}
	if ethReceived.Sign() > 0 {
		return "received", weiBigIntToEthFloat(ethReceived), "ETH"
	}
	if tr, ok := firstTokenTransfer(sentTokens); ok {
		return "sent", tokenAmountToFloat(tr.Amount, tr.Decimals), tr.Symbol
	}
	return "sent", weiBigIntToEthFloat(ethSent), "ETH"
}

func firstTokenTransfer(m map[string]erc20Transfer) (erc20Transfer, bool) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return erc20Transfer{}, false
	}
	sort.Strings(keys)
	return m[keys[0]], true
}

func formatEthTransferNotification(
	watchedAddr, from, to string, blockNum int64, success bool, explorerURL string,
	ethSentWei, ethReceivedWei *big.Int, sentTokens, receivedTokens map[string]erc20Transfer, gasWei *big.Int, isSender bool,
) string {
	hasSent := ethSentWei.Sign() > 0 || len(sentTokens) > 0
	hasReceived := ethReceivedWei.Sign() > 0 || len(receivedTokens) > 0

	var b strings.Builder
	switch {
	case hasSent && hasReceived:
		fmt.Fprintf(&b, "🔄 <b>ETH Wallet Activity</b>\n\n")
	case hasSent:
		fmt.Fprintf(&b, "📤 <b>ETH Sent</b>\n\n")
	default:
		fmt.Fprintf(&b, "📥 <b>ETH Received</b>\n\n")
	}

	if ethReceivedWei.Sign() > 0 {
		fmt.Fprintf(&b, "📥 Received: <b>%s ETH</b>\n", bigIntToDecimalString(ethReceivedWei, 18))
	}
	if ethSentWei.Sign() > 0 {
		fmt.Fprintf(&b, "📤 Sent: <b>%s ETH</b>\n", bigIntToDecimalString(ethSentWei, 18))
	}
	appendTokenLines(&b, "📥", receivedTokens)
	appendTokenLines(&b, "📤", sentTokens)

	if isSender && gasWei.Sign() > 0 {
		fmt.Fprintf(&b, "⛽ Gas: %s ETH\n", bigIntToDecimalString(gasWei, 18))
	}

	fmt.Fprintf(&b, "\nAddress: <code>%s</code>\n", truncateAddress(watchedAddr))
	fmt.Fprintf(&b, "From: <code>%s</code>\n", truncateAddress(from))
	if to != "" {
		fmt.Fprintf(&b, "To: <code>%s</code>\n", truncateAddress(to))
	}
	fmt.Fprintf(&b, "Block: %d\n", blockNum)
	if !success {
		fmt.Fprintf(&b, "Status: ❌ Failed\n")
	}
	fmt.Fprintf(&b, "\n<a href=\"%s\">View on Explorer</a>", explorerURL)
	return b.String()
}

func appendTokenLines(b *strings.Builder, prefix string, transfers map[string]erc20Transfer) {
	if len(transfers) == 0 {
		return
	}
	keys := make([]string, 0, len(transfers))
	for k := range transfers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		tr := transfers[k]
		symbol := tr.Symbol
		if symbol == "" {
			symbol = "TOKEN"
		}
		label := "Sent"
		if prefix == "📥" {
			label = "Received"
		}
		fmt.Fprintf(b, "%s %s: <b>%s %s</b>\n", prefix, label, bigIntToDecimalString(tr.Amount, tr.Decimals), symbol)
	}
}

func topicToAddress(topic string) string {
	topic = strings.TrimPrefix(strings.ToLower(topic), "0x")
	if len(topic) < 40 {
		return ""
	}
	return "0x" + topic[len(topic)-40:]
}

func decodeERC20Symbol(hexResult string) string {
	data := strings.TrimPrefix(hexResult, "0x")
	if len(data) == 0 {
		return ""
	}
	if len(data) >= 128 {
		lengthWord := data[64:128]
		l := int(hexToBigInt("0x" + lengthWord).Int64())
		start := 128
		end := start + l*2
		if l > 0 && end <= len(data) {
			raw := data[start:end]
			return strings.TrimSpace(string(hexToBytes(raw)))
		}
	}
	// bytes32 fallback
	raw := hexToBytes(data)
	raw = bytes.TrimRight(raw, "\x00")
	return strings.TrimSpace(string(raw))
}

func hexToBytes(s string) []byte {
	if len(s)%2 == 1 {
		s = "0" + s
	}
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		v, err := strconv.ParseUint(s[i:i+2], 16, 8)
		if err != nil {
			return nil
		}
		out = append(out, byte(v))
	}
	return out
}

func (m *matcher) ethRPCCall(ctx context.Context, method string, params []any, out any) error {
	reqBody := ethRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  method,
		Params:  params,
	}
	b, _ := json.Marshal(reqBody)
	rpcURL := m.cfg.EthRPC
	if !strings.HasPrefix(rpcURL, "http://") && !strings.HasPrefix(rpcURL, "https://") {
		rpcURL = "http://" + rpcURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ethHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var rpcResp ethRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return err
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("%s: %s", method, rpcResp.Error.Message)
	}
	if len(rpcResp.Result) == 0 || string(rpcResp.Result) == "null" {
		return nil
	}
	return json.Unmarshal(rpcResp.Result, out)
}

func ethEventType(dir string, isContract bool) string {
	if isContract {
		return "contract_interaction"
	}
	if dir == "out" {
		return "transfer_sent"
	}
	return "transfer_received"
}

func hexToInt64(s string) int64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimPrefix(s, "0x"), 16, 64)
	return v
}

func hexToBigInt(s string) *big.Int {
	if s == "" || s == "0x" {
		return big.NewInt(0)
	}
	out := new(big.Int)
	if _, ok := out.SetString(strings.TrimPrefix(strings.ToLower(s), "0x"), 16); !ok {
		return big.NewInt(0)
	}
	return out
}

func weiHexToEth(s string) float64 {
	if s == "" || s == "0x" {
		return 0
	}
	i := new(big.Int)
	i.SetString(strings.TrimPrefix(s, "0x"), 16)
	f := new(big.Float).SetInt(i)
	div := new(big.Float).SetFloat64(1e18)
	out, _ := new(big.Float).Quo(f, div).Float64()
	return out
}

func weiHexToEthString(s string) string {
	if s == "" || s == "0x" {
		return "0"
	}
	n := new(big.Int)
	if _, ok := n.SetString(strings.TrimPrefix(s, "0x"), 16); !ok {
		return "0"
	}

	base := big.NewInt(1_000_000_000_000_000_000) // 1e18
	intPart := new(big.Int).Div(n, base)
	fracPart := new(big.Int).Mod(n, base)
	if fracPart.Sign() == 0 {
		return intPart.String()
	}
	frac := fracPart.Text(10)
	if len(frac) < 18 {
		frac = strings.Repeat("0", 18-len(frac)) + frac
	}
	frac = strings.TrimRight(frac, "0")
	return intPart.String() + "." + frac
}

func weiBigIntToEthFloat(v *big.Int) float64 {
	if v == nil || v.Sign() == 0 {
		return 0
	}
	f := new(big.Float).SetInt(v)
	div := new(big.Float).SetFloat64(1e18)
	out, _ := new(big.Float).Quo(f, div).Float64()
	return out
}

func tokenAmountToFloat(v *big.Int, decimals int) float64 {
	if v == nil || v.Sign() == 0 {
		return 0
	}
	if decimals < 0 {
		decimals = 0
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	f := new(big.Float).SetInt(v)
	div := new(big.Float).SetInt(scale)
	out, _ := new(big.Float).Quo(f, div).Float64()
	return out
}

func bigIntToDecimalString(v *big.Int, decimals int) string {
	if v == nil || v.Sign() == 0 {
		return "0"
	}
	if decimals <= 0 {
		return v.String()
	}
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	intPart := new(big.Int).Div(v, base)
	fracPart := new(big.Int).Mod(v, base)
	if fracPart.Sign() == 0 {
		return intPart.String()
	}
	frac := fracPart.Text(10)
	if len(frac) < decimals {
		frac = strings.Repeat("0", decimals-len(frac)) + frac
	}
	frac = strings.TrimRight(frac, "0")
	return intPart.String() + "." + frac
}

func mulHex(a, b string) string {
	av := new(big.Int)
	bv := new(big.Int)
	if _, ok := av.SetString(strings.TrimPrefix(a, "0x"), 16); !ok {
		return "0x0"
	}
	if _, ok := bv.SetString(strings.TrimPrefix(b, "0x"), 16); !ok {
		return "0x0"
	}
	if av.Sign() == 0 || bv.Sign() == 0 {
		return "0x0"
	}
	return "0x" + new(big.Int).Mul(av, bv).Text(16)
}

func (m *matcher) processBlock(ctx context.Context, block *models.WsBlockNotify) {
	chainKey := fmt.Sprintf("%d->%d", block.Params.ChainFrom, block.Params.ChainTo)
	height := block.Params.Height

	if last, ok := m.lastSeen[chainKey]; ok {
		switch {
		case height > last+1:
			log.Printf("WARN possible missed blocks on chain %s: last=%d current=%d gap=%d block=%s", chainKey, last, height, height-last-1, block.Params.Hash)
		case height <= last:
			log.Printf("INFO non-monotonic block on chain %s: last=%d current=%d block=%s", chainKey, last, height, block.Params.Hash)
		}
	}
	m.lastSeen[chainKey] = height

	if m.cfg.VerboseLogs {
		log.Printf("DEBUG block received chain=%s height=%d hash=%s txs=%d", chainKey, height, block.Params.Hash, len(block.Params.Transactions))
	}

	queued := 0
	skippedNoInputs := 0
	skippedNotWatched := 0

	for _, tx := range block.Params.Transactions {
		if len(tx.Unsigned.Inputs) == 0 {
			skippedNoInputs++
			continue
		}

		if !m.txMatchesWatched(ctx, tx) {
			skippedNotWatched++
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
			queued++
		case <-ctx.Done():
			return
		}
	}

	if m.cfg.VerboseLogs {
		log.Printf("DEBUG block processed chain=%s height=%d queued=%d skipped_no_inputs=%d skipped_not_watched=%d", chainKey, height, queued, skippedNoInputs, skippedNotWatched)
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
	defer func() {
		matcherTxProcessDuration.Observe(time.Since(start).Seconds())
	}()

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
			if !matchesFilter(sub.Filter, isSender, hasSent, hasReceived, isContract) {
				continue
			}
			msgOut := msg
			if sub.Channel == models.ChannelTelegram {
				if label := m.store.GetAddressLabel(ctx, sub.ChatID, addr); label != "" {
					msgOut = fmt.Sprintf("🏷️ <b>%s</b>\n\n%s", html.EscapeString(label), msg)
				}
			}

			event, actionType, actionAmount, actionToken := deriveWebhookAction(flow, isSender, isContract)
			contractAddr := ""
			if len(flow.contractAddrs) > 0 {
				contractAddr = flow.contractAddrs[0]
			}

			notif := models.Notification{
				Channel:      sub.Channel,
				ChatID:       sub.ChatID,
				URL:          sub.URL,
				Message:      msgOut,
				Event:        event,
				ActionType:   actionType,
				ActionAmount: actionAmount,
				ActionToken:  actionToken,
				GasAmount:    flow.gasAlph,
				GasToken:     "ALPH",
				Address:      addr,
				Contract:     contractAddr,
				ChainFrom:    ref.GroupFrom,
				ChainTo:      ref.GroupTo,
				ExplorerURL:  fmt.Sprintf("%s/#/transactions/%s", m.cfg.ExplorerURL, tx.Hash),
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

func deriveWebhookAction(flow txFlow, isSender, isContract bool) (event, actionType string, actionAmount float64, actionToken string) {
	if isContract {
		event = "contract_interaction"
	} else if isSender {
		event = "transfer_sent"
	} else {
		event = "transfer_received"
	}

	if flow.receivedAlph > 0 {
		return event, "received", flow.receivedAlph, "ALPH"
	}
	for id, amt := range flow.receivedTokens {
		return event, "received", amt, flow.tokenSymbols[id]
	}
	if flow.sentAlph > 0 {
		return event, "sent", flow.sentAlph, "ALPH"
	}
	for id, amt := range flow.sentTokens {
		return event, "sent", amt, flow.tokenSymbols[id]
	}
	return event, "", 0, ""
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
