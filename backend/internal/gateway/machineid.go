package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	sdk "github.com/DouDOU-start/airgate-sdk/sdkgo"
)

var machineIDCache sync.Map // accountID -> string

func resolveMachineID(account *sdk.Account) string {
	if mid := account.Credentials["machine_id"]; mid != "" {
		return normalizeMachineID(mid)
	}

	if cached, ok := machineIDCache.Load(account.ID); ok {
		return cached.(string)
	}

	var derived string
	switch account.Type {
	case "api_key":
		derived = sha256Hex("KiroAPIKey/" + account.Credentials["kiro_api_key"])
	default:
		derived = sha256Hex("KotlinNativeAPI/" + account.Credentials["refresh_token"])
	}

	machineIDCache.Store(account.ID, derived)
	return derived
}

func normalizeMachineID(mid string) string {
	cleaned := strings.ReplaceAll(mid, "-", "")
	if len(cleaned) < 64 {
		for len(cleaned) < 64 {
			cleaned += cleaned
		}
		cleaned = cleaned[:64]
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
