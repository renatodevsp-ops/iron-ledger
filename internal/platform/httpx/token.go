package httpx

import "github.com/ironledger/iron-ledger/internal/sharedkernel/ids"

func newToken() (string, error) { return ids.NewToken(12) }
