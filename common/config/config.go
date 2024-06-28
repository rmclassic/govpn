package config

import "github.com/pchchv/govpn/common/cipher"

type Config struct {
	Key          string
	CIDR         string
	Protocol     string
	LocalAddr    string
	ServerAddr   string
	ServerMode   bool
	DataChannels int
}

func (config *Config) Init() {
	cipher.GenerateKey(config.Key)
}
