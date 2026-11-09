package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/bsv8/bitcoin-libp2p/signer"
	"github.com/bsv8/keymaster.cc-cli/internal/appconfig"
	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
	"github.com/bsv8/keymaster.cc-cli/internal/ssp"
)

type runtimeServices struct {
	SatSuppliers        []appconfig.Supplier
	ArbitrationServices []arbitration.Endpoint
}

const defaultArbiterPublicKey = "036258d4f23285e868cd91ad0a84c7ec4d798dc6e3965e3f7575d9c0dca4fcac"
const defaultArbiterPeer = "16Uiu2HAmKGtsKHXsLAdaneryb5r7L6G6Adg8QctE9yK3GGycbNB"

func resolveServices(info appconfig.Info, settings appconfig.Settings) (runtimeServices, error) {
	if info.BsvNetwork != "main" && info.BsvNetwork != "test" {
		return runtimeServices{}, fmt.Errorf("服务目录需要明确的钱包网络")
	}
	domain, name := "arbitration.bsv8.com", "BSV8 主网仲裁"
	if info.BsvNetwork == "test" {
		domain, name = "arbitration-test.bsv8.com", "BSV8 测试网仲裁"
	}
	out := runtimeServices{
		SatSuppliers: []appconfig.Supplier{{
			ID: appconfig.DefaultSupplierID, Name: appconfig.DefaultSupplierName,
			PublicKeyHex: appconfig.DefaultSupplierPublicKeyHex,
			Multiaddrs:   []string{defaultSupplierAddress(info.BsvNetwork)},
			Enabled:      true,
		}},
		ArbitrationServices: []arbitration.Endpoint{{
			Name: name, PublicKeyHex: defaultArbiterPublicKey,
			Address: "/dns4/" + domain + "/tcp/443/tls/ws/p2p/" + defaultArbiterPeer,
		}},
	}
	for _, s := range settings.Services.SatSuppliers {
		if s.ID != appconfig.DefaultSupplierID {
			out.SatSuppliers = append(out.SatSuppliers, s)
		}
	}
	out.ArbitrationServices = append(out.ArbitrationServices, settings.Services.ArbitrationServices...)
	if os.Getenv("KEYMASTER_ENV") == "dev" {
		if raw, ok := os.LookupEnv("KEYMASTER_DEV_ARBITRATION_SERVICES"); ok {
			var endpoints []arbitration.Endpoint
			if err := json.Unmarshal([]byte(raw), &endpoints); err != nil || endpoints == nil {
				return runtimeServices{}, fmt.Errorf("KEYMASTER_DEV_ARBITRATION_SERVICES 必须是 JSON 数组")
			}
			out.ArbitrationServices = endpoints
		}
		if raw, ok := os.LookupEnv("KEYMASTER_DEV_SAT_SUPPLIER"); ok {
			var value struct {
				Name         string   `json:"name"`
				PublicKeyHex string   `json:"publicKeyHex"`
				Multiaddrs   []string `json:"multiaddrs"`
			}
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				return runtimeServices{}, fmt.Errorf("KEYMASTER_DEV_SAT_SUPPLIER: %w", err)
			}
			if value.Name == "" {
				value.Name = "dev 网关"
			}
			if len(value.Multiaddrs) == 0 {
				return runtimeServices{}, fmt.Errorf("dev 网关需要非空 multiaddrs")
			}
			for _, address := range value.Multiaddrs {
				if _, err := (arbitration.Endpoint{PublicKeyHex: value.PublicKeyHex, Address: address}).AddressInfo(); err != nil {
					return runtimeServices{}, fmt.Errorf("dev 网关: %w", err)
				}
			}
			out.SatSuppliers[0] = appconfig.Supplier{ID: appconfig.DefaultSupplierID, Name: value.Name, PublicKeyHex: value.PublicKeyHex, Multiaddrs: value.Multiaddrs, Enabled: true}
		}
	}
	// A code-owned default is not a forced publisher: the first enabled
	// custom gateway keeps its configured priority and the built-in gateway
	// stays as a supplement. Download and sell loops always dial the first
	// enabled entry, so without this the custom (and e2e local) supplier
	// would never be used.
	for _, configured := range settings.Services.SatSuppliers {
		if !configured.Enabled {
			continue
		}
		if configured.ID != appconfig.DefaultSupplierID {
			for i := 1; i < len(out.SatSuppliers); i++ {
				if out.SatSuppliers[i].ID == configured.ID {
					selected := out.SatSuppliers[i]
					copy(out.SatSuppliers[1:i+1], out.SatSuppliers[:i])
					out.SatSuppliers[0] = selected
					break
				}
			}
		}
		break
	}
	return out, nil
}

func loadRuntimeServices(info appconfig.Info, settings appconfig.Settings) (runtimeServices, error) {
	return resolveServices(info, settings)
}

func defaultSupplierAddress(network string) string {
	if network == "main" {
		return appconfig.DefaultSupplierMainnet
	}
	return appconfig.DefaultSupplierTestnet
}

func dialSSP(ctx context.Context, gateway string, s signer.Signer) (*ssp.Client, error) {
	return ssp.Dial(ctx, ssp.Options{Gateway: gateway, Signer: s})
}

func sspPublish(c *ssp.Client) func(ctx context.Context, channel string, contentJSON []byte) error {
	return func(ctx context.Context, channel string, contentJSON []byte) error {
		_, err := c.Publish(ctx, channel, contentJSON)
		return err
	}
}
