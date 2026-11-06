package download

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"

	masterseed "github.com/bsv8/MasterSeed"

	"github.com/bsv8/keymaster.cc-cli/internal/store"
	"github.com/bsv8/keymaster.cc-download-scheduler/scheduler"
)

// localFileInfo 携带一次本地重验证得到的文件结构；只用于创建新任务，
// 历史付款记录不能替代字节级重验。清单与复用只看 Seed 字节与哈希，绝不
// 使用卖方声明的文件长度。
type localFileInfo struct {
	BlockHashes       []string
	ReusedBlockHashes []string
}

// sharedSchedulers 在进程内按配置复用调度器实例：同一配置的多个任务共用
// 一个实例；配置变化时新建，不影响已创建任务（配置已复制进快照）。
var sharedSchedulers = struct {
	sync.Mutex
	instances map[string]*scheduler.Scheduler
}{instances: make(map[string]*scheduler.Scheduler)}

func sharedScheduler(limits scheduler.Config) (*scheduler.Scheduler, error) {
	key := limits.MaxBlockPrice + "/" + strconv.FormatInt(limits.BaseSeats, 10) + "/" +
		strconv.FormatInt(limits.FloatingSeats, 10) + "/" + strconv.FormatInt(limits.PoolBlocks, 10) +
		"/" + strconv.FormatInt(limits.RequestTimeoutMs, 10)
	sharedSchedulers.Lock()
	defer sharedSchedulers.Unlock()
	if sched, ok := sharedSchedulers.instances[key]; ok {
		return sched, nil
	}
	sched, err := scheduler.New(limits)
	if err != nil {
		return nil, err
	}
	sharedSchedulers.instances[key] = sched
	return sched, nil
}

// quarantineStaging 将校验失败的暂存对象移出读取路径（保留一份备查），
// 否则重买的正确内容无法落盘（SaveExact 禁止冲突覆盖）。付款与会话
// 证据不动，不削弱不可覆盖规则。零字节损坏同样隔离；隔离或移除失败
// 返回错误，调用方不得静默继续采购。
func quarantineStaging(workStore *store.Store, name string, data []byte) error {
	if workStore == nil {
		return errors.New("bitfs: store is required for quarantine")
	}
	target := "staging/quarantine/" + name
	if err := workStore.Save(target, data); err != nil {
		return fmt.Errorf("bitfs: quarantine %s: %w", name, err)
	}
	if err := workStore.Remove("staging/" + name); err != nil {
		return fmt.Errorf("bitfs: remove corrupt %s: %w", name, err)
	}
	return nil
}

// verifyLocalFile 重新验证本地 staging 中的 Seed 与全部 Block，返回内核
// 创建任务所需的 Hash 清单与复用集合。Seed 只验哈希，清单按 Seed 字节长度
// 逐项读出，块只验哈希；卖方声明长度缺失或不一致不影响本路径。种子缺失或
// 损坏时返回 ok=false（按新购买处理）；单个块损坏或缺失时仅从复用清单中
// 剔除，不影响其余内容。损坏的暂存对象会被隔离（见 quarantineStaging），
// 以免阻止重买内容的落盘。
func verifyLocalFile(workStore *store.Store, seedHashHex string) (localFileInfo, bool, error) {
	if workStore == nil {
		return localFileInfo{}, false, nil
	}
	seed, err := workStore.Load("staging/seed")
	if err != nil {
		return localFileInfo{}, false, nil
	}
	expected, err := masterseed.ParseDigestHex(seedHashHex)
	if err != nil {
		return localFileInfo{}, false, fmt.Errorf("bitfs: 无效的 Seed Hash: %w", err)
	}
	if _, err := masterseed.VerifySeed(context.Background(), bytes.NewReader(seed), expected); err != nil {
		// 种子损坏或与目标不符：隔离后按新购买处理，否则重买的 Seed 无法落盘。
		if qerr := quarantineStaging(workStore, "seed", seed); qerr != nil {
			return localFileInfo{}, false, qerr
		}
		return localFileInfo{}, false, nil
	}
	if uint64(len(seed))%uint64(masterseed.DigestSize) != 0 {
		if qerr := quarantineStaging(workStore, "seed", seed); qerr != nil {
			return localFileInfo{}, false, qerr
		}
		return localFileInfo{}, false, nil
	}
	blockCount := uint64(len(seed)) / uint64(masterseed.DigestSize)
	info := localFileInfo{
		BlockHashes: make([]string, 0, blockCount),
	}
	seenReuse := map[string]bool{}
	for index := uint64(0); index < blockCount; index++ {
		hash, err := masterseed.ReadBlockHash(context.Background(), bytes.NewReader(seed), uint64(len(seed)), index)
		if err != nil {
			return localFileInfo{}, false, nil
		}
		hashHex := hex.EncodeToString(hash.Bytes())
		info.BlockHashes = append(info.BlockHashes, hashHex)
		if seenReuse[hashHex] {
			continue
		}
		block, err := workStore.Load("staging/" + hashHex)
		if err != nil {
			continue
		}
		if _, err := masterseed.VerifyBlock(context.Background(), block, hash); err != nil {
			if qerr := quarantineStaging(workStore, hashHex, block); qerr != nil {
				return localFileInfo{}, false, qerr
			}
			continue
		}
		seenReuse[hashHex] = true
		info.ReusedBlockHashes = append(info.ReusedBlockHashes, hashHex)
	}
	return info, true, nil
}

// verifyLocalBlocks 在 Seed 已知后重新验证本地块（与创建时同一套哈希校验），
// 供 manifest 事件携带 reusedBlockHashes 使用。
func verifyLocalBlocks(workStore *store.Store, seedHashHex string) []string {
	info, ok, err := verifyLocalFile(workStore, seedHashHex)
	if err != nil || !ok {
		return nil
	}
	return info.ReusedBlockHashes
}
