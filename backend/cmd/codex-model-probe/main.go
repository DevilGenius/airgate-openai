// codex-model-probe sends an unchanged model ID directly to Codex over WebSocket.
// AIRGATE_API_KEY selects an authorized group; OAuth credentials stay in memory.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"

	"github.com/DevilGenius/airgate-openai/backend/internal/gateway"
)

type config struct {
	Database struct {
		Host     string `yaml:"host"`
		Port     int    `yaml:"port"`
		User     string `yaml:"user"`
		Password string `yaml:"password"`
		DBName   string `yaml:"dbname"`
		SSLMode  string `yaml:"sslmode"`
	} `yaml:"database"`
}

type report struct {
	AccountID       int     `json:"account_id"`
	SentModel       string  `json:"sent_model"`
	Endpoint        string  `json:"endpoint"`
	HandshakeStatus int     `json:"handshake_status"`
	ResponseModel   string  `json:"response_model"`
	ResponseStatus  string  `json:"response_status"`
	ResponseID      string  `json:"response_id"`
	Text            string  `json:"text"`
	Error           string  `json:"error,omitempty"`
	Seconds         float64 `json:"seconds"`
}

func run() error {
	configPath := flag.String("config", "../../airgate-core/backend/config.yaml", "Core configuration (read only)")
	accountID := flag.Int("account", 8380, "OAuth account ID in the API key's group")
	model := flag.String("model", "codex-auto-review", "Exact upstream model; never rewritten")
	reportPath := flag.String("report", "", "Optional sanitized JSON report")
	flag.Parse()
	key := strings.TrimSpace(os.Getenv("AIRGATE_API_KEY"))
	if key == "" {
		return errors.New("AIRGATE_API_KEY is required")
	}
	raw, err := os.ReadFile(*configPath)
	if err != nil {
		return err
	}
	var cfg config
	if err = yaml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	dbURL := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(cfg.Database.Host, strconv.Itoa(cfg.Database.Port)), Path: "/" + cfg.Database.DBName, User: url.UserPassword(cfg.Database.User, cfg.Database.Password)}
	q := dbURL.Query()
	q.Set("sslmode", cfg.Database.SSLMode)
	dbURL.RawQuery = q.Encode()
	db, err := sql.Open("postgres", dbURL.String())
	if err != nil {
		return errors.New("database open failed")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return errors.New("read-only database connection failed")
	}
	defer tx.Rollback()
	sum := sha256.Sum256([]byte(key))
	var credentialsJSON, proxyJSON []byte
	var proxySlot sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT a.credentials, COALESCE(to_jsonb(p), '{}'::jsonb), a.proxy_slot
 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id
 JOIN api_keys k ON k.group_api_keys=ag.group_id
 LEFT JOIN proxies p ON p.id=a.account_proxy
 WHERE a.id=$1 AND k.key_hash=$2 AND k.status='active'
 AND (k.expires_at IS NULL OR k.expires_at > NOW())
 AND a.deleted_at IS NULL AND a.state='active'`, *accountID, hex.EncodeToString(sum[:])).Scan(&credentialsJSON, &proxyJSON, &proxySlot)
	if err != nil {
		return fmt.Errorf("account lookup failed (must be active and in the key's group): %w", err)
	}
	var credentials map[string]string
	if err = json.Unmarshal(credentialsJSON, &credentials); err != nil {
		return errors.New("invalid account credentials")
	}
	token := credentials["access_token"]
	if token == "" || credentials["api_key"] != "" {
		return errors.New("selected account is not an OAuth access-token account")
	}
	var proxy struct {
		Mode, Protocol, Address, Username, Password, Status string
		Port                                                int
	}
	if err = json.Unmarshal(proxyJSON, &proxy); err != nil {
		return errors.New("invalid proxy configuration")
	}
	proxyURL := credentials["proxy_url"]
	if proxy.Address != "" {
		if proxy.Status != "active" {
			return errors.New("probe requires an active proxy")
		}
		if proxy.Mode == "group" {
			if !proxySlot.Valid {
				return errors.New("proxy group requires an account slot")
			}
			proxy.Username = fmt.Sprintf("%04x", proxySlot.Int64)
		}
		u := &url.URL{Scheme: proxy.Protocol, Host: net.JoinHostPort(proxy.Address, strconv.Itoa(proxy.Port))}
		if proxy.Username != "" {
			u.User = url.UserPassword(proxy.Username, proxy.Password)
		}
		proxyURL = u.String()
	}
	_ = tx.Rollback()
	session := uuid.NewString()
	started := time.Now()
	out := report{AccountID: *accountID, SentModel: *model, Endpoint: gateway.ChatGPTWSURL}
	conn, response, err := gateway.DialWebSocket(ctx, gateway.WSConfig{
		Token: token, AccountID: credentials["chatgpt_account_id"], ProxyURL: proxyURL,
		SessionID: session, Originator: "codex_cli_rs",
		Headers: http.Header{"X-Codex-Beta-Features": {"remote_compaction_v2"}},
	})
	if response != nil {
		out.HandshakeStatus = response.StatusCode
	}
	if err != nil {
		return errors.New("Codex WebSocket connection failed; credentials and proxy details omitted")
	}
	defer conn.Close()
	// Deliberately do not call buildWSRequest, resolveEffectiveModel or Forward.
	request := map[string]any{
		"type": "response.create", "model": *model, "stream": true, "store": false,
		"instructions": "You are a helpful assistant.",
		"input":        []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply with exactly OK."}}}},
	}
	if err = conn.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}
	if err = conn.WriteJSON(request); err != nil {
		return errors.New("Codex request write failed")
	}
	result := gateway.ReceiveWSResponse(ctx, conn, nil)
	// Read model from the raw upstream terminal event, not a gateway model fallback.
	terminal := result.CompletedEventRaw
	if len(terminal) == 0 {
		terminal = result.FailedEventRaw
	}
	out.ResponseModel = gjson.GetBytes(terminal, "response.model").String()
	out.ResponseStatus = gjson.GetBytes(terminal, "response.status").String()
	out.ResponseID = result.ResponseID
	out.Text = result.Text
	if result.Err != nil {
		out.Error = result.Err.Error()
	}
	out.Seconds = time.Since(started).Seconds()
	rendered, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	safe := string(rendered)
	for _, secret := range []string{key, token, credentials["refresh_token"], proxy.Password} {
		if secret != "" {
			safe = strings.ReplaceAll(safe, secret, "[REDACTED]")
		}
	}
	fmt.Println(safe)
	if *reportPath != "" {
		if err = os.WriteFile(*reportPath, []byte(safe+"\n"), 0600); err != nil {
			return err
		}
	}
	if result.Err != nil || out.ResponseStatus != "completed" {
		return errors.New("upstream request did not complete")
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
