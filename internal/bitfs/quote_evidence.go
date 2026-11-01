package bitfs

import (
	"errors"
	"os"

	"github.com/bsv8/keymaster.cc-cli/internal/store"
)

// requestQuote 保存后的请求只能使用自己的报价；只有迁移前已签历史
// 没有逐请求报价文件时，才从原池元数据读取原始证据。不可用最新报价替换。
func requestQuote(s *store.Store, meta *PoolMeta, authorizationID string) ([]byte, error) {
	raw, err := s.Load(poolPath(meta.PoolID, kindName("kind1", authorizationID)))
	if err == nil {
		return raw, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return meta.QuoteRaw()
}
