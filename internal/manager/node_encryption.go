package manager

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

const maxVLESSPaddingGapMS = 60_000

// Match the pinned core's VLESS Encryption grammar, while checking keys and
// padding before a node can replace the working runtime configuration. Xray's
// JSON builder alone does not validate all of these runtime requirements.
func validateVLESSEncryption(value string) error {
	if value == "none" {
		return nil
	}
	parts := strings.Split(value, ".")
	if len(parts) < 4 || parts[0] != "mlkem768x25519plus" {
		return fmt.Errorf("VLESS encryption 格式无效")
	}
	if parts[1] != "native" && parts[1] != "xorpub" && parts[1] != "random" {
		return fmt.Errorf("VLESS encryption 混淆模式无效")
	}
	if parts[2] != "1rtt" && parts[2] != "0rtt" {
		return fmt.Errorf("VLESS encryption 握手模式无效")
	}
	keyCount, paddingCount, maxPadding, maxGapMS := 0, 0, 0, 0
	for _, part := range parts[3:] {
		if len(part) < 20 {
			if keyCount != 0 {
				return fmt.Errorf("VLESS encryption padding 必须位于公钥之前")
			}
			triple := strings.Split(part, "-")
			if len(triple) != 3 {
				return fmt.Errorf("VLESS encryption padding 格式无效")
			}
			var n [3]int
			for i, field := range triple {
				if field == "" || strings.IndexFunc(field, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
					return fmt.Errorf("VLESS encryption padding 数值无效")
				}
				parsed, err := strconv.Atoi(field)
				if err != nil {
					return fmt.Errorf("VLESS encryption padding 数值无效")
				}
				n[i] = parsed
			}
			if n[0] > 100 || n[1] > n[2] || (paddingCount == 0 && (n[0] != 100 || n[1] < 35)) {
				return fmt.Errorf("VLESS encryption padding 范围无效")
			}
			if paddingCount%2 == 0 {
				if n[2] > 65553-maxPadding {
					return fmt.Errorf("VLESS encryption padding 总长度超过上限")
				}
				maxPadding += n[2]
			} else {
				// The pinned core sleeps between fragments without observing the
				// request context. Bound the sum of all possible delays, including
				// optional gaps, before accepting subscription-controlled padding.
				if n[2] > maxVLESSPaddingGapMS-maxGapMS {
					return fmt.Errorf("VLESS encryption padding 总间隔不能超过 60 秒")
				}
				maxGapMS += n[2]
			}
			paddingCount++
			continue
		}
		key, err := base64.RawURLEncoding.Strict().DecodeString(part)
		if err != nil || base64.RawURLEncoding.EncodeToString(key) != part {
			return fmt.Errorf("VLESS encryption 公钥必须为无填充 Base64URL")
		}
		switch len(key) {
		case 32:
			pub, err := ecdh.X25519().NewPublicKey(key)
			if err != nil {
				return fmt.Errorf("VLESS encryption X25519 公钥无效")
			}
			// A fixed, nonsecret probe rejects low-order public keys that would
			// otherwise fail only when the first connection performs ECDH.
			probe, _ := ecdh.X25519().NewPrivateKey(make([]byte, 32))
			if _, err := probe.ECDH(pub); err != nil {
				return fmt.Errorf("VLESS encryption X25519 公钥无效")
			}
		case 1184:
			if _, err := mlkem.NewEncapsulationKey768(key); err != nil {
				return fmt.Errorf("VLESS encryption ML-KEM-768 公钥无效")
			}
		default:
			return fmt.Errorf("VLESS encryption 公钥必须为 32 或 1184 字节")
		}
		keyCount++
	}
	if keyCount == 0 {
		return fmt.Errorf("VLESS encryption 缺少公钥")
	}
	return nil
}

func validShadowsocks2022Method(method string) bool {
	switch method {
	case "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		return true
	}
	return false
}

func validateShadowsocksPassword(method, password string) error {
	if !validShadowsocks2022Method(method) {
		return nil
	}
	keyBytes := 32
	if method == "2022-blake3-aes-128-gcm" {
		keyBytes = 16
	}
	keys := strings.Split(password, ":")
	if method == "2022-blake3-chacha20-poly1305" && len(keys) != 1 {
		return fmt.Errorf("SS2022 ChaCha20 不支持身份密钥链")
	}
	for _, key := range keys {
		decoded, err := base64.StdEncoding.Strict().DecodeString(key)
		if err != nil || len(decoded) != keyBytes || base64.StdEncoding.EncodeToString(decoded) != key {
			return fmt.Errorf("SS2022 密钥必须为标准 Base64 编码的 %d 字节", keyBytes)
		}
	}
	return nil
}
