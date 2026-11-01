package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	bitfsdebug "github.com/bsv8/go-bitfs/debug"
	schedulerdebug "github.com/bsv8/keymaster.cc-download-scheduler/scheduler"
	managerdebug "github.com/bsv8/keymaster.cc-utxo-manager/utxo"
)

func componentDebugStatus() any {
	return map[string]bool{"utxo-manager": managerdebug.DebugEnabled(), "trade-scheduler": schedulerdebug.DebugEnabled()}
}

// dev 诊断只向 stderr 输出 JSONL；文件保留由操作者/测试进程负责。
func configureDebug() {
	managerdebug.SetDebugSink(nil)
	schedulerdebug.SetDebugSink(nil)
	if os.Getenv("KEYMASTER_ENV") != "dev" {
		bitfsdebug.SetSink(nil)
		return
	}
	var output sync.Mutex
	bitfsdebug.SetSink(func(event bitfsdebug.Event) {
		raw, err := json.Marshal(event)
		if err != nil {
			return
		}
		output.Lock()
		defer output.Unlock()
		_, _ = fmt.Fprintf(os.Stderr, "[bitfs-debug] %s\n", raw)
	})
	managerdebug.SetDebugSink(func(event managerdebug.DebugEvent) {
		bitfsdebug.Log(event.Component, event.Event, func() any { return event.Details })
	})
	schedulerdebug.SetDebugSink(func(event schedulerdebug.DebugEvent) {
		bitfsdebug.Log(event.Component, event.Event, func() any { return event.Details })
	})
}
