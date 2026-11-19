package daemon

import (
	"context"
	"time"

	bitfsdebug "github.com/bsv8/BitFSProtocol/debug"
	"github.com/bsv8/BitFSProtocol/protocol"
	"github.com/bsv8/keymaster.cc-cli/internal/woc"
)

func bitfsChainFacts(ctx context.Context, client *woc.Client) protocol.Facts {
	height, err := client.ChainHeight(ctx)
	if err != nil {
		bitfsdebug.Log("keymaster.cc-cli", "chain.height.failed", func() any { return err })
	}
	return protocol.Facts{Now: time.Now(), BlockHeight: protocol.BlockHeight(height)}
}
