package provider

import (
	"fmt"
	"github.com/containous/traefik/safe"
	"github.com/containous/traefik/types"
	"github.com/docker/libkv/store"
	"github.com/docker/libkv/store/consul"
)

var _ Provider = (*Consul)(nil)

// Consul holds configurations of the Consul provider.
type Consul struct {
	Kv `mapstructure:",squash"`
}

// Provide allows the provider to provide configurations to traefik
// using the given configuration channel.
func (provider *Consul) Provide(configurationChan chan<- types.ConfigMessage, pool *safe.Pool, constraints []types.Constraint) error {
	store, err := provider.CreateStore()
	if err != nil {
		return fmt.Errorf("Failed to Connect to KV store: %v", err)
	}
	provider.kvclient = store
	return provider.provide(configurationChan, pool, constraints)
}

// CreateStore creates the KV store
func (provider *Consul) CreateStore() (store.Store, error) {
	provider.storeType = store.CONSUL
	consul.Register()
	return provider.createStore()
}
