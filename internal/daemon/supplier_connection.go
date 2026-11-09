package daemon

import (
	"context"
	"fmt"
	"sync"

	"github.com/bsv8/bitcoin-libp2p/signer"
	"github.com/bsv8/keymaster.cc-cli/internal/ssp"
)

type supplierConnection struct {
	client *ssp.Client
	users  int
}

// acquireSupplier shares one authenticated SSP connection between downloads and
// selling. Two libp2p hosts with the same wallet identity can displace each
// other at the gateway. The last user closes the connection and its signer.
func (d *Daemon) acquireSupplier(ctx context.Context, gateway string) (*ssp.Client, func(), error) {
	d.supplierMu.Lock()
	defer d.supplierMu.Unlock()
	if d.suppliers == nil {
		d.suppliers = make(map[string]*supplierConnection)
	}
	connection := d.suppliers[gateway]
	if connection == nil {
		d.mu.Lock()
		key := append([]byte(nil), d.privateKey...)
		d.mu.Unlock()
		identity, err := signer.NewLocalSigner(key)
		clear(key)
		if err != nil {
			return nil, nil, err
		}
		client, err := dialSSP(ctx, gateway, identity)
		if err != nil {
			return nil, nil, err
		}
		connection = &supplierConnection{client: client}
		d.suppliers[gateway] = connection
	}
	if err := connection.client.Err(); err != nil {
		return nil, nil, fmt.Errorf("supplier connection unavailable: %w", err)
	}
	connection.users++
	var once sync.Once
	release := func() {
		once.Do(func() {
			d.supplierMu.Lock()
			defer d.supplierMu.Unlock()
			connection.users--
			if connection.users == 0 {
				delete(d.suppliers, gateway)
				_ = connection.client.Close()
			}
		})
	}
	return connection.client, release, nil
}
