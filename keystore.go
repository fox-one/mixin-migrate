package main

import (
	"encoding/json"
	"os"

	"github.com/fox-one/mixin-sdk-go/v2"
)

type Keystore struct {
	mixin.Keystore
	Pin      string `json:"pin"`
	SpendKey string `json:"spend_key,omitempty"`
}

func loadKeystore(path string) (*Keystore, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer f.Close()

	var key Keystore
	if err := json.NewDecoder(f).Decode(&key); err != nil {
		return nil, err
	}

	return &key, nil
}
