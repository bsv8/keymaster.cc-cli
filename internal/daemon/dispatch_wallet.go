package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	d.initMu.Lock()
	defer d.initMu.Unlock()
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if ctx.Err() != nil {
		return nil, errDetail(kmapi.ErrStopping, "初始化请求已取消")
	}
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
	d.initMu.Lock()
	defer d.initMu.Unlock()
	if e := d.needRunning(); e != nil {
		return nil, e
	}
	if ctx.Err() != nil {
		return nil, errDetail(kmapi.ErrStopping, "初始化请求已取消")
	}
	if p.Password == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "密码不能为空")
	}
	if strings.TrimSpace(p.KeyMaterial) == "" {
		return nil, errDetail(kmapi.ErrBadRequest, "keyMaterial 必填")
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
	if d.layout.HasAnyWallet() {
		return errDetail(kmapi.ErrConflict, "已有钱包材料，拒绝覆盖")
	}
	if err := d.layout.EnsureDirs(); err != nil {
		return errDetail(kmapi.ErrInternal, err.Error())
	}
	// The daemon owns the directory lock and serializes initialization. Exclusive
	// creation is still used for the Key so an existing wallet is never replaced.
	keyFile, err := os.OpenFile(d.layout.KeyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errDetail(kmapi.ErrInternal, "创建钱包文件失败: "+err.Error())
	}
	_, writeErr := keyFile.Write(keyRaw)
	if err := errors.Join(writeErr, keyFile.Close()); err != nil {
		return errDetail(kmapi.ErrInternal, errors.Join(err, os.Remove(d.layout.KeyPath)).Error())
	}
	store := appconfig.New(d.layout.Dir)
	if err := store.Init(network, pubHex, auth); err != nil {
		return errDetail(kmapi.ErrInternal, "初始化失败: "+errors.Join(err, os.Remove(d.layout.KeyPath)).Error())
	}
	// 重载后台状态为已锁定（需解锁后可用）。
	tasks := newTaskManager(d.layout, store)
	tasks.events = d.events
	tasks.onTerminal = func() { go d.refreshBalance("task-terminal") }
	roots := newRootsManager(d.layout, store)
	collectors := newCollectorsManager(d.layout.Dir, store, d.collectorSign)
	if err := roots.openAll(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "km: 初始化售卖目录: %v\n", err)
	}
	d.mu.Lock()
	d.store = store
	d.publicKey = pubHex
	if addr, err := addressFor(pubHex, network == "main"); err == nil {
		d.address = addr
	}
	d.tasks = tasks
	d.roots = roots
	d.collectors = collectors
	if d.state != StateStopping {
		d.state = StateLocked
	}
	d.mu.Unlock()
	d.publishWallet()
	return nil
}
