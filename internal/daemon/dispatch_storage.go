package daemon

import (
	"context"
	"strings"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/keymaster.cc-cli/internal/appstorage"
)

func (d *Daemon) storageIdentity() (network, wallet string, errDetail *kmapi.ErrorDetail) {
	if d.store == nil {
		return "", "", errDetail2(kmapi.ErrNotInitialized, "尚未初始化")
	}
	// 停止期间拒绝新业务：调用者在 Drain 前只会看到 STOPPING。
	if d.isStopping() {
		return "", "", errDetail2(kmapi.ErrStopping, "后台停止中")
	}
	d.mu.Lock()
	locked := d.state != StateUnlocked
	walletPub := d.publicKey
	d.mu.Unlock()
	if locked {
		return "", "", errDetail2(kmapi.ErrLocked, "后台已锁定，请先解锁")
	}
	if d.storage == nil {
		return "", "", errDetail2(kmapi.ErrInternal, "App Storage 未就绪")
	}
	info, _, err := d.store.Snapshot()
	if err != nil {
		return "", "", errDetail2(kmapi.ErrInternal, err.Error())
	}
	return info.BsvNetwork, walletPub, nil
}

func errDetail2(code, msg string) *kmapi.ErrorDetail {
	return &kmapi.ErrorDetail{Code: code, Message: msg}
}

func (d *Daemon) opStoragePut(ctx context.Context, p kmapi.StoragePutRequest, attachments map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail) {
	network, wallet, e := d.storageIdentity()
	if e != nil {
		return nil, nil, e
	}
	// 命名空间隔离：请求必须指向当前钱包与网络，不自动迁移。
	if p.Network != "" && p.Network != network {
		return nil, nil, errDetail2(kmapi.ErrBadRequest, "网络不匹配")
	}
	if p.WalletPublicKey != "" && !strings.EqualFold(p.WalletPublicKey, wallet) {
		return nil, nil, errDetail2(kmapi.ErrBadRequest, "钱包不匹配")
	}
	if strings.TrimSpace(p.AppPublicKey) == "" || strings.TrimSpace(p.Path) == "" {
		return nil, nil, errDetail2(kmapi.ErrBadRequest, "app/路径必填")
	}
	content, ok := attachments["content"]
	if !ok {
		// 兼容单附件：若仅有一个附件则取之。
		for _, v := range attachments {
			content = v
			ok = true
			break
		}
	}
	if !ok {
		return nil, nil, errDetail2(kmapi.ErrBadRequest, "附件缺失")
	}
	mime := ""
	if p.Mime != nil {
		mime = *p.Mime
	}
	ifNone := ""
	if p.IfNoneMatch != nil {
		ifNone = *p.IfNoneMatch
	}
	rev, updatedAt, err := d.storage.Put(network, wallet, p.AppPublicKey, p.Path, content, mime, ifNone, 0)
	if err != nil {
		switch err {
		case appstorage.ErrConflict:
			return nil, nil, errDetail2(kmapi.ErrConflict, "条件写冲突")
		case appstorage.ErrTooLarge:
			return nil, nil, errDetail2(kmapi.ErrTooLarge, "对象过大")
		case appstorage.ErrInvalidPath:
			return nil, nil, errDetail2(kmapi.ErrBadRequest, "路径非法")
		default:
			return nil, nil, errDetail2(kmapi.ErrInternal, err.Error())
		}
	}
	return kmapi.StoragePutResponse{Revision: rev, UpdatedAt: &updatedAt}, nil, nil
}

func (d *Daemon) opStorageGet(ctx context.Context, p kmapi.StorageGetRequest) (any, map[string][]byte, *kmapi.ErrorDetail) {
	network, wallet, e := d.storageIdentity()
	if e != nil {
		return nil, nil, e
	}
	if p.Network != "" && p.Network != network {
		return nil, nil, errDetail2(kmapi.ErrBadRequest, "网络不匹配")
	}
	if p.WalletPublicKey != "" && !strings.EqualFold(p.WalletPublicKey, wallet) {
		return nil, nil, errDetail2(kmapi.ErrBadRequest, "钱包不匹配")
	}
	var offset, length int64
	if p.Offset != nil {
		offset = *p.Offset
	}
	length = -1
	if p.Length != nil {
		length = *p.Length
	}
	obj, err := d.storage.Get(network, wallet, p.AppPublicKey, p.Path, offset, length)
	if err != nil {
		switch err {
		case appstorage.ErrNotFound:
			return nil, nil, errDetail2(kmapi.ErrNotFound, "对象不存在")
		default:
			if strings.Contains(err.Error(), "范围越界") {
				return nil, nil, errDetail2(kmapi.ErrBadRequest, "范围越界")
			}
			return nil, nil, errDetail2(kmapi.ErrInternal, err.Error())
		}
	}
	// 取总数用于 eof 计算。
	full, err := d.storage.Get(network, wallet, p.AppPublicKey, p.Path, 0, -1)
	if err != nil {
		return nil, nil, errDetail2(kmapi.ErrInternal, err.Error())
	}
	total := int64(len(full.Content))
	eof := offset+int64(len(obj.Content)) >= total
	resp := kmapi.StorageGetResponse{Revision: obj.Revision}
	if obj.MIME != "" {
		m := obj.MIME
		resp.Mime = &m
	}
	if p.Offset != nil || p.Length != nil {
		o := offset
		resp.Offset = &o
		resp.TotalSize = &total
		e := eof
		resp.Eof = &e
	}
	return resp, map[string][]byte{"content": obj.Content}, nil
}

func (d *Daemon) opStorageList(ctx context.Context, p kmapi.StorageListRequest) (any, *kmapi.ErrorDetail) {
	network, wallet, e := d.storageIdentity()
	if e != nil {
		return nil, e
	}
	if p.Network != "" && p.Network != network {
		return nil, errDetail2(kmapi.ErrBadRequest, "网络不匹配")
	}
	if p.WalletPublicKey != "" && !strings.EqualFold(p.WalletPublicKey, wallet) {
		return nil, errDetail2(kmapi.ErrBadRequest, "钱包不匹配")
	}
	limit := 0
	if p.Limit != nil {
		limit = *p.Limit
	}
	cursor := ""
	if p.Cursor != nil {
		cursor = *p.Cursor
	}
	entries, next, err := d.storage.List(network, wallet, p.AppPublicKey, p.Prefix, cursor, limit)
	if err != nil {
		if err == appstorage.ErrInvalidCursor {
			return nil, errDetail2(kmapi.ErrBadRequest, "游标无效")
		}
		return nil, errDetail2(kmapi.ErrInternal, err.Error())
	}
	out := make([]map[string]any, 0, len(entries))
	for _, en := range entries {
		out = append(out, map[string]any{"path": en.Path, "mime": en.MIME, "revision": en.Revision, "size": en.Size})
	}
	resp := kmapi.StorageListResponse{Entries: out}
	if next != "" {
		resp.NextCursor = &next
	}
	return resp, nil
}

func (d *Daemon) opStorageDelete(ctx context.Context, p kmapi.StorageDeleteRequest) (any, *kmapi.ErrorDetail) {
	network, wallet, e := d.storageIdentity()
	if e != nil {
		return nil, e
	}
	if p.Network != "" && p.Network != network {
		return nil, errDetail2(kmapi.ErrBadRequest, "网络不匹配")
	}
	if p.WalletPublicKey != "" && !strings.EqualFold(p.WalletPublicKey, wallet) {
		return nil, errDetail2(kmapi.ErrBadRequest, "钱包不匹配")
	}
	if err := d.storage.Delete(network, wallet, p.AppPublicKey, p.Path); err != nil {
		if err == appstorage.ErrNotFound {
			return nil, errDetail2(kmapi.ErrNotFound, "对象不存在")
		}
		return nil, errDetail2(kmapi.ErrInternal, err.Error())
	}
	return kmapi.Ok{Ok: true}, nil
}
