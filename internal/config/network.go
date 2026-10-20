package config

// Network is the BSV network selected at first run.
type Network string

// Supported networks.
const (
	NetworkMain Network = "main"
	NetworkTest Network = "test"
)

// Valid reports whether the network is supported.
func (n Network) Valid() bool {
	return n == NetworkMain || n == NetworkTest
}

// Mainnet reports whether the network is BSV mainnet.
func (n Network) Mainnet() bool { return n == NetworkMain }

// WOCBaseURL returns the WhatsonChain API base URL derived from the network.
// The URL is intentionally not configurable in v1.
func (n Network) WOCBaseURL() string {
	if n == NetworkMain {
		return "https://api.whatsonchain.com/v1/bsv/main"
	}
	return "https://api.whatsonchain.com/v1/bsv/test"
}
