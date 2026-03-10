package models

import "fmt"

// Explorer API types

type TokenTransfer struct {
	ID     string `json:"id"`
	Amount string `json:"amount"`
}

type TxInput struct {
	OutputRef struct {
		Hint int    `json:"hint"`
		Key  string `json:"key"`
	} `json:"outputRef"`
	UnlockScript   string          `json:"unlockScript"`
	TxHashRef      string          `json:"txHashRef"`
	Address        string          `json:"address"`
	AttoAlphAmount string          `json:"attoAlphAmount"`
	Tokens         []TokenTransfer `json:"tokens,omitempty"`
}

type TxOutput struct {
	Type           string          `json:"type"`
	Hint           int             `json:"hint"`
	Key            string          `json:"key"`
	AttoAlphAmount string          `json:"attoAlphAmount"`
	Address        string          `json:"address"`
	Tokens         []TokenTransfer `json:"tokens,omitempty"`
	Message        string          `json:"message"`
	Spent          string          `json:"spent"`
}

type Transaction struct {
	Type              string     `json:"type"`
	Hash              string     `json:"hash"`
	BlockHash         string     `json:"blockHash"`
	Timestamp         int64      `json:"timestamp"`
	Inputs            []TxInput  `json:"inputs"`
	Outputs           []TxOutput `json:"outputs"`
	GasAmount         int        `json:"gasAmount"`
	GasPrice          string     `json:"gasPrice"`
	ScriptExecutionOk bool       `json:"scriptExecutionOk"`
	Coinbase          bool       `json:"coinbase"`
}

// Fullnode WebSocket types

type WsBlockNotify struct {
	Method  string      `json:"method"`
	Params  BlockParams `json:"params"`
	Jsonrpc string      `json:"jsonrpc"`
}

type BlockParams struct {
	Hash         string    `json:"hash"`
	Timestamp    int64     `json:"timestamp"`
	ChainFrom   int       `json:"chainFrom"`
	ChainTo     int       `json:"chainTo"`
	Height      int       `json:"height"`
	Deps        []string  `json:"deps"`
	Transactions []BlockTx `json:"transactions"`
	Nonce        string    `json:"nonce"`
	Version      int       `json:"version"`
	DepStateHash string    `json:"depStateHash"`
	TxsHash      string    `json:"txsHash"`
	Target       string    `json:"target"`
	GhostUncles  []any     `json:"ghostUncles"`
}

type BlockOutput struct {
	Type           string          `json:"type,omitempty"`
	Hint           int             `json:"hint"`
	Key            string          `json:"key"`
	AttoAlphAmount string          `json:"attoAlphAmount"`
	Address        string          `json:"address"`
	Tokens         []TokenTransfer `json:"tokens"`
	LockTime       int64           `json:"lockTime"`
	Message        string          `json:"message"`
}

type BlockTx struct {
	Unsigned struct {
		TxID         string        `json:"txId"`
		Version      int           `json:"version"`
		NetworkID    int           `json:"networkId"`
		GasAmount    int           `json:"gasAmount"`
		GasPrice     string        `json:"gasPrice"`
		Inputs       []any         `json:"inputs"`
		FixedOutputs []BlockOutput `json:"fixedOutputs"`
	} `json:"unsigned"`
	ScriptExecutionOk bool          `json:"scriptExecutionOk"`
	ContractInputs    []any         `json:"contractInputs"`
	GeneratedOutputs  []BlockOutput `json:"generatedOutputs"`
	InputSignatures   []any         `json:"inputSignatures"`
	ScriptSignatures  []any         `json:"scriptSignatures"`
}

// Inter-service types

type TxRef struct {
	ID        string `json:"id"`
	GroupFrom int    `json:"group_from"`
	GroupTo   int    `json:"group_to"`
	Height    int    `json:"height"`
}

const (
	ChannelTelegram = "telegram"
	ChannelWebhook  = "webhook"
)

type Notification struct {
	Channel      string  `json:"channel"`
	ChatID       int64   `json:"chat_id,omitempty"`
	URL          string  `json:"url,omitempty"`
	Message      string  `json:"message"`
	Event        string  `json:"event,omitempty"`
	ActionType   string  `json:"action_type,omitempty"`
	ActionAmount float64 `json:"action_amount,omitempty"`
	ActionToken  string  `json:"action_token,omitempty"`
	GasAmount    float64 `json:"gas_amount,omitempty"`
	GasToken     string  `json:"gas_token,omitempty"`
	Address      string  `json:"address,omitempty"`
	Contract     string  `json:"contract,omitempty"`
	ChainFrom    int     `json:"chainfrom,omitempty"`
	ChainTo      int     `json:"chainto,omitempty"`
	ExplorerURL  string  `json:"explorer_url,omitempty"`
}

// WebhookEvent is the JSON envelope POSTed to user webhook URLs.
type WebhookEvent struct {
	ID          string           `json:"id"`
	Type        string           `json:"type"`
	SpecVersion string           `json:"specversion"`
	Timestamp   string           `json:"timestamp"`
	Data      WebhookEventData `json:"data"`
}

type WebhookAction struct {
	Type   string  `json:"type,omitempty"`
	Amount float64 `json:"amount,omitempty"`
	Token  string  `json:"token,omitempty"`
}

type WebhookGas struct {
	Amount float64 `json:"amount,omitempty"`
	Token  string  `json:"token,omitempty"`
}

type WebhookEventData struct {
	Event       string        `json:"event,omitempty"`
	Action      WebhookAction `json:"action,omitempty"`
	Gas         WebhookGas    `json:"gas,omitempty"`
	Address     string        `json:"address,omitempty"`
	Contract    string        `json:"contract,omitempty"`
	ChainFrom   int           `json:"chainfrom,omitempty"`
	ChainTo     int           `json:"chainto,omitempty"`
	ExplorerURL string        `json:"explorer_url,omitempty"`
	MessageHTML string        `json:"message_html,omitempty"`
	Channel     string        `json:"channel,omitempty"`
}

// Constants

const AttoAlphDivisor = 1e18

// ParseFloat parses a numeric string into float64.
func ParseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}
