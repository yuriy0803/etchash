module etchash

go 1.17

require (
	github.com/edsrzf/mmap-go v1.1.0
	github.com/ethereum/go-ethereum v0.0.0-00010101000000-000000000000
	github.com/hashicorp/golang-lru v0.5.5-0.20210104140557-80c98217689d
	golang.org/x/crypto v0.3.0
	golang.org/x/sys v0.2.0
)

require (
	github.com/btcsuite/btcd/btcec/v2 v2.2.0 // indirect
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.0.1 // indirect
	github.com/go-stack/stack v1.8.0 // indirect
)

replace github.com/ethereum/go-ethereum => github.com/etclabscore/core-geth v1.12.8

replace github.com/hashicorp/golang-lru => github.com/yuriy0803/golang-lru v0.0.0-20221118130757-9f9597820c48
