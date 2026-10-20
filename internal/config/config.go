// Package config manages ~/.km/config.json: first-run initialization,
// network-bound defaults, strict loading and atomic saving.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bsv8/keymaster.cc-cli/internal/wallet"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// Version is the config schema version.
const Version = 1

// Default supplier identity (bsv8 gateway); the address differs per network.
const (
	DefaultSupplierID           = "bsv8"
	DefaultSupplierName         = "bsv8"
	DefaultSupplierPublicKeyHex = "031e6690e2c12ef4f7b96204d6c96652625f7f300606f01f17f7b089c076425918"
)

// Default supplier gateway addresses from keymaster.cc.
const (
	DefaultSupplierMainnet = "/dns/us-gateway.bsv8.com/tcp/443/tls/ws/p2p/16Uiu2HAmEhfHraPUP1YrYQFy6M8TTbtCZq6A7BGcXi77ZwCS5jxj"
	DefaultSupplierTestnet = "/dns/ustest-gateway.bsv8.com/tcp/443/tls/ws/p2p/16Uiu2HAmEhfHraPUP1YrYQFy6M8TTbtCZq6A7BGcXi77ZwCS5jxj"
)

// Price defaults per network (sats). The server default applies to files
// without an individual price; seed and full block use the same value.
var defaultPrices = map[Network]string{
	NetworkMain: "10000",
	NetworkTest: "100",
}

// Scheduler holds the buyer scheduling kernel settings.
type Scheduler struct {
	MaxBlockPriceSatoshis string `json:"maxBlockPriceSatoshis"`
	PoolBlocks            int    `json:"poolBlocks"`
	TotalSeats            int    `json:"totalSeats"`
	FloatingSeats         int    `json:"floatingSeats"`
	RequestTimeoutMs      int    `json:"requestTimeoutMs"`
}

// Supplier is one SatSubscription gateway entry.
type Supplier struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	PublicKeyHex string   `json:"publicKeyHex"`
	Multiaddrs   []string `json:"multiaddrs"`
	Enabled      bool     `json:"enabled"`
}

// Services holds the external services the CLI depends on.
type Services struct {
	SatSuppliers []Supplier `json:"satSuppliers"`
}

// Server holds the seller settings.
type Server struct {
	DefaultSeedPriceSatoshis      string `json:"defaultSeedPriceSatoshis"`
	DefaultFullBlockPriceSatoshis string `json:"defaultFullBlockPriceSatoshis"`
	// ArbiterPublicKeys are the compressed arbiter public keys this server
	// offers in quotes. At least one is required to serve: every pool is
	// 2-of-3 and the buyer must pick an arbiter from the quote.
	ArbiterPublicKeys []string `json:"arbiterPublicKeys,omitempty"`
}

// Admin holds the local Web Admin settings.
type Admin struct {
	APIKey string `json:"apiKey"`
}

// Config is the on-disk configuration.
type Config struct {
	Version    int                   `json:"version"`
	BsvNetwork Network               `json:"bsvNetwork"`
	Wallet     wallet.NetworkKeyHold `json:"wallet"`
	WOCAPIKey  string                `json:"wocApiKey"`
	// FeeRateSatoshisPerKb is the miner fee rate used for wallet spends.
	FeeRateSatoshisPerKb int64     `json:"feeRateSatoshisPerKb"`
	Scheduler            Scheduler `json:"scheduler"`
	Services             Services  `json:"services"`
	Server               Server    `json:"server"`
	Admin                Admin     `json:"admin"`
}

// DefaultPath returns ~/.km/config.json.
func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("无法定位用户主目录: %w", err)
	}
	return filepath.Join(home, ".km", "config.json"), nil
}

// Init creates a new config with a freshly generated secp256k1 key.
func Init(path string, network Network, password string) (*Config, error) {
	privateKey, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, fmt.Errorf("生成私钥失败: %w", err)
	}
	serialized := privateKey.Serialize()
	defer clear(serialized)
	return InitWithKey(path, network, password, serialized)
}

// InitWithKey creates a config for an explicitly provided 32-byte secp256k1
// key (imported or test keys). It performs the same defaults and validation as
// Init.
func InitWithKey(path string, network Network, password string, privateKey []byte) (*Config, error) {
	if !network.Valid() {
		return nil, fmt.Errorf("网络必须是 main 或 test")
	}
	if password == "" {
		return nil, errors.New("钱包密码不能为空")
	}
	if len(privateKey) != 32 {
		return nil, errors.New("私钥必须是 32 字节")
	}
	serialized := append([]byte(nil), privateKey...)
	defer clear(serialized)
	document, err := wallet.Create(serialized, string(network), "km wallet", password, 0)
	if err != nil {
		return nil, err
	}
	apiKey, err := randomToken()
	if err != nil {
		return nil, err
	}
	price := defaultPrices[network]
	cfg := &Config{
		Version:              Version,
		BsvNetwork:           network,
		Wallet:               *document,
		WOCAPIKey:            "",
		FeeRateSatoshisPerKb: 1000,
		Scheduler: Scheduler{
			MaxBlockPriceSatoshis: "0",
			PoolBlocks:            4,
			TotalSeats:            8,
			FloatingSeats:         2,
			RequestTimeoutMs:      5000,
		},
		Services: Services{
			SatSuppliers: []Supplier{{
				ID:           DefaultSupplierID,
				Name:         DefaultSupplierName,
				PublicKeyHex: DefaultSupplierPublicKeyHex,
				Multiaddrs:   []string{defaultSupplierAddress(network)},
				Enabled:      true,
			}},
		},
		Server: Server{
			DefaultSeedPriceSatoshis:      price,
			DefaultFullBlockPriceSatoshis: price,
		},
		Admin: Admin{APIKey: apiKey},
	}
	if err := cfg.Save(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Load reads and strictly validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var shell struct {
		Version              int             `json:"version"`
		BsvNetwork           Network         `json:"bsvNetwork"`
		Wallet               json.RawMessage `json:"wallet"`
		WOCAPIKey            string          `json:"wocApiKey"`
		FeeRateSatoshisPerKb int64           `json:"feeRateSatoshisPerKb"`
		Scheduler            Scheduler       `json:"scheduler"`
		Services             Services        `json:"services"`
		Server               Server          `json:"server"`
		Admin                Admin           `json:"admin"`
	}
	if err := json.Unmarshal(raw, &shell); err != nil {
		return nil, fmt.Errorf("配置文件不是有效 JSON: %w", err)
	}
	if shell.Version != Version {
		return nil, fmt.Errorf("配置版本不受支持: %d", shell.Version)
	}
	if !shell.BsvNetwork.Valid() {
		return nil, fmt.Errorf("配置网络无效: %q", shell.BsvNetwork)
	}
	document, err := wallet.Parse(shell.Wallet)
	if err != nil {
		return nil, fmt.Errorf("配置中的钱包无效: %w", err)
	}
	if document.BsvNetwork != string(shell.BsvNetwork) {
		return nil, errors.New("配置网络与钱包网络绑定不一致")
	}
	if shell.FeeRateSatoshisPerKb == 0 {
		// Configs created before the fee-rate field adopt the default.
		shell.FeeRateSatoshisPerKb = 1000
	}
	cfg := &Config{
		Version:              shell.Version,
		BsvNetwork:           shell.BsvNetwork,
		Wallet:               *document,
		WOCAPIKey:            shell.WOCAPIKey,
		FeeRateSatoshisPerKb: shell.FeeRateSatoshisPerKb,
		Scheduler:            shell.Scheduler,
		Services:             shell.Services,
		Server:               shell.Server,
		Admin:                shell.Admin,
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save(path string) error {
	if err := c.validate(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	encoded, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(directory, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		temporary.Close()
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	return nil
}

var satoshiPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func (c *Config) validate() error {
	if c.Version != Version {
		return fmt.Errorf("配置版本不受支持: %d", c.Version)
	}
	if !c.BsvNetwork.Valid() {
		return fmt.Errorf("配置网络无效: %q", c.BsvNetwork)
	}
	if c.Wallet.Format != wallet.Format || c.Wallet.BsvNetwork != string(c.BsvNetwork) {
		return errors.New("配置钱包与网络不匹配")
	}
	if c.Wallet.KeyHold.Format == "" || c.Wallet.KeyHold.PublicKeyHex == "" {
		return errors.New("配置缺少钱包文档")
	}
	if c.Admin.APIKey == "" {
		return errors.New("配置缺少管理 API Key")
	}
	if c.FeeRateSatoshisPerKb < 1 || c.FeeRateSatoshisPerKb > 1_000_000 {
		return errors.New("手续费率无效")
	}
	if !satoshiPattern.MatchString(c.Scheduler.MaxBlockPriceSatoshis) {
		return errors.New("调度器缺省块价无效")
	}
	if c.Scheduler.PoolBlocks < 1 || c.Scheduler.TotalSeats < 2 || c.Scheduler.FloatingSeats < 1 ||
		c.Scheduler.FloatingSeats >= c.Scheduler.TotalSeats || c.Scheduler.RequestTimeoutMs < 1 {
		return errors.New("调度器设置无效")
	}
	for _, price := range []string{c.Server.DefaultSeedPriceSatoshis, c.Server.DefaultFullBlockPriceSatoshis} {
		if !satoshiPattern.MatchString(price) {
			return errors.New("server 缺省售价无效")
		}
	}
	if len(c.Services.SatSuppliers) == 0 {
		return errors.New("配置缺少 SatSubscription 供应商")
	}
	for _, supplier := range c.Services.SatSuppliers {
		if supplier.ID == "" || !regexp.MustCompile(`^(02|03)[0-9a-f]{64}$`).MatchString(supplier.PublicKeyHex) || len(supplier.Multiaddrs) == 0 {
			return fmt.Errorf("供应商配置无效: %s", supplier.ID)
		}
	}
	if strings.Contains(c.WOCAPIKey, "\n") {
		return errors.New("WOC API Key 无效")
	}
	return nil
}

// ClientUnresolvedDir returns the client-level unresolved history directory
// (~/.km/client/unresolved by default). It is deliberately independent of any
// seed-hash working directory.
func ClientUnresolvedDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "client", "unresolved")
}

func defaultSupplierAddress(network Network) string {
	if network == NetworkMain {
		return DefaultSupplierMainnet
	}
	return DefaultSupplierTestnet
}

func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("生成随机值失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer), nil
}
