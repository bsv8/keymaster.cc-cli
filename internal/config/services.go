package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/bsv8/keymaster.cc-cli/internal/arbitration"
)

const defaultArbiterPublicKey = "036258d4f23285e868cd91ad0a84c7ec4d798dc6e3965e3f75755d9c0dca4fc4ac"
const defaultArbiterPeer = "16Uiu2HAmKGtsKHXsLAdaneryb5r7L6G6Adg8QctE9yKx3GGycbNB"

// RuntimeServices resolves code-owned main/test defaults plus explicit custom
// entries. Dev overrides are process-local, never saved into config.json.
// Dev is a deployment mode, not a wallet network.
func (c *Config) RuntimeServices() (Services, error) {
	if !c.BsvNetwork.Valid() {
		return Services{}, fmt.Errorf("服务目录需要明确的钱包网络")
	}
	domain, name := "arbitration.bsv8.com", "BSV8 主网仲裁"
	if c.BsvNetwork == NetworkTest {
		domain, name = "arbitration-test.bsv8.com", "BSV8 测试网仲裁"
	}
	services := Services{
		SatSuppliers: []Supplier{{ID: DefaultSupplierID, Name: DefaultSupplierName, PublicKeyHex: DefaultSupplierPublicKeyHex,
			Multiaddrs: []string{defaultSupplierAddress(c.BsvNetwork)}, Enabled: true}},
		ArbitrationServices: []arbitration.Endpoint{{Name: name, PublicKeyHex: defaultArbiterPublicKey,
			Address: "/dns4/" + domain + "/tcp/443/tls/ws/p2p/" + defaultArbiterPeer}},
	}
	for _, supplier := range c.Services.SatSuppliers {
		if supplier.ID != DefaultSupplierID {
			services.SatSuppliers = append(services.SatSuppliers, supplier)
		}
	}
	services.ArbitrationServices = append(services.ArbitrationServices, c.Services.ArbitrationServices...)
	if os.Getenv("KEYMASTER_ENV") == "dev" {
		if raw, ok := os.LookupEnv("KEYMASTER_DEV_ARBITRATION_SERVICES"); ok {
			var endpoints []arbitration.Endpoint
			if err := json.Unmarshal([]byte(raw), &endpoints); err != nil || endpoints == nil {
				return Services{}, fmt.Errorf("KEYMASTER_DEV_ARBITRATION_SERVICES 必须是 JSON 数组")
			}
			services.ArbitrationServices = endpoints
		}
		if raw, ok := os.LookupEnv("KEYMASTER_DEV_SAT_SUPPLIER"); ok {
			var value struct {
				Name         string   `json:"name"`
				PublicKeyHex string   `json:"publicKeyHex"`
				Multiaddrs   []string `json:"multiaddrs"`
			}
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				return Services{}, fmt.Errorf("KEYMASTER_DEV_SAT_SUPPLIER: %w", err)
			}
			if value.Name == "" {
				value.Name = "dev 网关"
			}
			if len(value.Multiaddrs) == 0 {
				return Services{}, fmt.Errorf("dev 网关需要非空 multiaddrs")
			}
			for _, address := range value.Multiaddrs {
				if _, err := (arbitration.Endpoint{PublicKeyHex: value.PublicKeyHex, Address: address}).AddressInfo(); err != nil {
					return Services{}, fmt.Errorf("dev 网关: %w", err)
				}
			}
			services.SatSuppliers[0] = Supplier{ID: DefaultSupplierID, Name: value.Name, PublicKeyHex: value.PublicKeyHex, Multiaddrs: value.Multiaddrs, Enabled: true}
		}
	}
	keys := map[string]bool{}
	for _, endpoint := range services.ArbitrationServices {
		if _, err := endpoint.AddressInfo(); err != nil {
			return Services{}, err
		}
		if keys[endpoint.PublicKeyHex] {
			return Services{}, fmt.Errorf("仲裁目录公钥重复: %s", endpoint.PublicKeyHex)
		}
		keys[endpoint.PublicKeyHex] = true
	}
	// A code-owned default is not a forced publisher: preserve the existing
	// explicit choice of a custom gateway (first enabled configured entry).
	// Dev replaces the default identity only, just as in the browser wallet.
	for _, configured := range c.Services.SatSuppliers {
		if !configured.Enabled {
			continue
		}
		if configured.ID != DefaultSupplierID {
			for i := 1; i < len(services.SatSuppliers); i++ {
				if services.SatSuppliers[i].ID == configured.ID {
					selected := services.SatSuppliers[i]
					copy(services.SatSuppliers[1:i+1], services.SatSuppliers[:i])
					services.SatSuppliers[0] = selected
					break
				}
			}
		}
		break
	}
	return services, nil
}

// RuntimeArbiterPublicKeys always includes directory identities in seller
// quotes. Additional explicitly accepted keys remain available, as in wallet.
func (c *Config) RuntimeArbiterPublicKeys() ([]string, error) {
	services, err := c.RuntimeServices()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(services.ArbitrationServices)+len(c.Server.ArbiterPublicKeys))
	seen := map[string]bool{}
	for _, endpoint := range services.ArbitrationServices {
		seen[endpoint.PublicKeyHex] = true
		keys = append(keys, endpoint.PublicKeyHex)
	}
	for _, key := range c.Server.ArbiterPublicKeys {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys, nil
}
