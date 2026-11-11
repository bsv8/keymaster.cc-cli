package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	keyhold "github.com/bsv8/KeyHold/go/v2"
	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

func hmacBinding(priv []byte, network, pubHex string) string {
	mac := hmac.New(sha256.New, priv)
	_, _ = mac.Write([]byte("keymaster.network-identity.v1:" + network + ":" + pubHex))
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *Daemon) opWalletCreate(ctx context.Context, p kmapi.WalletCreateRequest) (any, *kmapi.ErrorDetail) {
	if p.Password == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "密码不能为空")
	}
	network := strings.ToLower(strings.TrimSpace(p.Network))
	if network != "test" && network != "main" {
		return nil, errDetail(kmapi.ErrBadRequest, "网络必须是 test 或 main")
	}
	if d.layout.HasInitMarkers() {
		return nil, errDetail(kmapi.ErrConflict, "已经初始化，不覆盖")
	}
	if d.layout.HasAnyWallet() {
		return nil, errDetail(kmapi.ErrConflict, "数据目录存在半套或损坏文件，不能当成首次安装")
	}
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, "生成私钥失败")
	}
	serialized := key.Serialize()
	raw, err := keyhold.ExportPrivateKey(keyhold.PrivateKeyExportInput{PrivateKey: serialized, Password: p.Password, Label: "km wallet", Parameters: keyhold.RecommendedParameters()})
	for i := range serialized {
		serialized[i] = 0
	}
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, "创建 KeyHold 失败")
	}
	doc, err := keyhold.Parse(raw)
	if err != nil {
		return nil, errDetail(kmapi.ErrInternal, "解析 KeyHold 失败")
	}
	if _, err := keyhold.Unlock(doc, p.Password); err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, "密码验证失败")
	}
	pubHex := doc.PublicKeyHex
	unlocked, err := keyhold.Unlock(doc, p.Password)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, "密码错误")
	}
	auth := hmacBinding(unlocked.PrivateKey, network, unlocked.PublicKeyHex)
	for i := range unlocked.PrivateKey {
		unlocked.PrivateKey[i] = 0
	}
	if err := d.persistNewWallet(network, pubHex, auth, raw); err != nil {
		return nil, err
	}
	return kmapi.WalletCreateResponse{Ok: true}, nil
}

func (d *Daemon) opWalletImport(ctx context.Context, p kmapi.WalletImportRequest) (any, *kmapi.ErrorDetail) {
	if p.Password == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "密码不能为空")
	}
	if strings.TrimSpace(p.KeyMaterial) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "keyMaterial 必填")
	}
	network := "test"
	// keyMaterial 可能含 network 字段？首版 network 固定 test，调用方通过设置切换？按 spec WalletImportRequest 只有 password+keyMaterial，先用 test。
	if d.layout.HasInitMarkers() {
		return nil, errDetail(kmapi.ErrConflict, "已经初始化，不覆盖")
	}
	if d.layout.HasAnyWallet() {
		return nil, errDetail(kmapi.ErrConflict, "数据目录存在半套或损坏文件，不能当成首次安装")
	}
	raw := []byte(p.KeyMaterial)
	doc, err := keyhold.Parse(raw)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, "导入文件不是有效的纯 KeyHold")
	}
	unlocked, err := keyhold.Unlock(doc, p.Password)
	if err != nil {
		return nil, errDetail(kmapi.ErrBadRequest, "解密失败（密码错误或文件损坏）")
	}
	auth := hmacBinding(unlocked.PrivateKey, network, unlocked.PublicKeyHex)
	pubHex := unlocked.PublicKeyHex
	for i := range unlocked.PrivateKey {
		unlocked.PrivateKey[i] = 0
	}
	if err := d.persistNewWallet(network, pubHex, auth, raw); err != nil {
		return nil, err
	}
	return kmapi.WalletImportResponse{Ok: true}, nil
}

func (d *Daemon) persistNewWallet(network, pubHex, auth string, keyRaw []byte) *kmapi.ErrorDetail {
	// 先写 info/settings（可恢复标记），再写 key.json；失败清理半套。
	store := appconfig.New(d.layout.Dir)
	if err := store.Init(network, pubHex, auth); err != nil {
		return errDetail(kmapi.ErrInternal, "初始化失败: "+err.Error())
	}
	var v any
	if err := json.Unmarshal(keyRaw, &v); err != nil {
		_ = os.Remove(d.layout.InfoPath)
		_ = os.Remove(d.layout.SettingsPath)
		return errDetail(kmapi.ErrBadRequest, "导入文件无效")
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		_ = os.Remove(d.layout.InfoPath)
		_ = os.Remove(d.layout.SettingsPath)
		return errDetail(kmapi.ErrInternal, err.Error())
	}
	if err := os.WriteFile(d.layout.KeyPath, pretty, 0o600); err != nil {
		_ = os.Remove(d.layout.InfoPath)
		_ = os.Remove(d.layout.SettingsPath)
		_ = os.Remove(d.layout.KeyPath)
		return errDetail(kmapi.ErrInternal, err.Error())
	}
	// 重载后台状态为已锁定（需解锁后可用）。
	d.store = store
	d.publicKey = pubHex
	if addr, err := addressFor(pubHex, network == "main"); err == nil {
		d.address = addr
	}
	d.mu.Lock()
	d.state = StateLocked
	d.mu.Unlock()
	d.tasks = newTaskManager(d.layout, store)
	d.tasks.events = d.events
	d.roots = newRootsManager(d.layout, store)
	d.collectors = newCollectorsManager(d.layout.Dir, store, d.collectorSign)
	_ = d.roots.openAll(context.Background())
	_ = fmt.Sprintf
	return nil
}
