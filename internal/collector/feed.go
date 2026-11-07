package collector

import (
	"strconv"

	"github.com/bsv8/keymaster.cc-cli/internal/catalog"
)

// FeedEntries 将已建立映射的完整文件交给采集队列：同 Hash 多路径取
// 最小 RelativePath 为准并合并为最新；目录路径不上报。
// FeedEntries queues mapped files; same-hash paths collapse to the smallest.
func FeedEntries(s *Service, entries []*catalog.Entry, mediaType func(name string) string) {
	best := map[string]*catalog.Entry{}
	for _, entry := range entries {
		if entry == nil || entry.State != "ready" || entry.SeedHashHex == "" {
			continue
		}
		if prev, ok := best[entry.SeedHashHex]; !ok || entry.RelativePath < prev.RelativePath {
			best[entry.SeedHashHex] = entry
		}
	}
	for _, entry := range best {
		s.NotifyStored(Record{
			SeedHashHex:   entry.SeedHashHex,
			FileName:      entry.Name,
			FileSizeBytes: strconv.FormatUint(entry.Size, 10),
			MediaType:     mediaType(entry.Name),
		})
	}
}
