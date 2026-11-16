package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/bsv8/KeymasterAPIProtocol/kmapi"
	"github.com/bsv8/WebLoom/go/webloom"
	"github.com/bsv8/keymaster.cc-cli/pluginmanifest"
)

// APIProtocol 是唯一外部入口；WebLoom 不额外开放万能 RPC。
type apiRequest struct {
	params      json.RawMessage
	attachments map[string][]byte
}
type apiResponse struct {
	value       any
	attachments map[string][]byte
	problem     *kmapi.ErrorDetail
}
type apiHandler func(context.Context, json.RawMessage, map[string][]byte) (any, map[string][]byte, *kmapi.ErrorDetail)

func apiCapability(method string) webloom.Capability {
	return webloom.Capability{Kind: "rpc", ID: method, Version: "v1"}
}

func apiPlugin(id string, methods map[string]apiHandler) webloom.Plugin {
	// 使用共享目录核对方法归属，防止前后台插件划分漂移。
	var expected []string
	for _, unit := range pluginmanifest.Units() {
		if unit.PluginID == id {
			expected = unit.Methods
			break
		}
	}
	capabilities := make([]webloom.Capability, 0, len(methods))
	for method := range methods {
		capabilities = append(capabilities, apiCapability(method))
	}
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i].ID < capabilities[j].ID })
	return webloom.Plugin{
		Manifest: webloom.Manifest{PluginID: id, UnitID: id + ".daemon", Provides: capabilities},
		Setup: func(ctx *webloom.Context) error {
			if len(expected) != len(methods) {
				return fmt.Errorf("插件 %s 的方法目录不一致", id)
			}
			for _, method := range expected {
				if methods[method] == nil {
					return fmt.Errorf("插件 %s 缺少方法 %s", id, method)
				}
			}
			for method, execute := range methods {
				if err := ctx.Handle(apiCapability(method), func(call context.Context, raw any) (any, error) {
					request := raw.(apiRequest)
					started := time.Now()
					if os.Getenv("KEYMASTER_ENV") == "dev" {
						defer func() {
							log.Printf("[plugin.call] plugin=%s unit=%s instance=%s method=%s elapsed=%s cancelled=%t", ctx.PluginID(), ctx.UnitID(), ctx.InstanceID(), method, time.Since(started), call.Err() != nil)
						}()
					}
					value, attachments, problem := execute(call, request.params, request.attachments)
					return apiResponse{value, attachments, problem}, nil
				}); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// finishOpen 在公开 Daemon 前完成插件装配；独立后台和内嵌后台使用同一实例。
func (d *Daemon) finishOpen() (*Daemon, error) {
	runtime, err := webloom.Start(context.Background(), []webloom.Plugin{
		d.vaultAPIPlugin(), d.sellAPIPlugin(), d.collectorAPIPlugin(), d.downloadAPIPlugin(),
		d.storageAPIPlugin(), d.appsAPIPlugin(), d.protocolAPIPlugin(), d.backgroundAPIPlugin(),
	})
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	d.plugins = runtime
	return d, nil
}
