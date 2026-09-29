package manager

import (
	"context"
	"crypto/ecdh"
	"crypto/mlkem"
	"encoding/base64"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func encryptionTestX25519(t *testing.T) string {
	t.Helper()
	key, err := ecdh.X25519().NewPrivateKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
}

func TestVLESSEncryptionValidation(t *testing.T) {
	key := encryptionTestX25519(t)
	kem, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatal(err)
	}
	kemKey := base64.RawURLEncoding.EncodeToString(kem.EncapsulationKey().Bytes())
	for _, value := range []string{
		"none",
		"mlkem768x25519plus.native.1rtt." + key,
		"mlkem768x25519plus.xorpub.0rtt." + kemKey + "." + key,
		"mlkem768x25519plus.random.1rtt.100-35-100.50-0-10.50-0-3333." + key,
	} {
		if err := validateVLESSEncryption(value); err != nil {
			t.Fatalf("valid encryption rejected: %v", err)
		}
	}
	prefix := "mlkem768x25519plus.native.1rtt."
	for name, value := range map[string]string{
		"empty": "", "unknown": "aes128", "mode": "mlkem768x25519plus.unknown.1rtt." + key,
		"rtt":         "mlkem768x25519plus.native.2rtt." + key,
		"missing key": prefix + "100-35-35", "empty key": prefix,
		"wrong size": prefix + base64.RawURLEncoding.EncodeToString(make([]byte, 31)),
		"padded key": prefix + key + "=", "newline key": prefix + key + "\n",
		"low order":              prefix + base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"invalid kem":            prefix + base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("\xff", 1184))),
		"padding after key":      prefix + key + ".100-35-35",
		"extra padding field":    prefix + "100-35-35-0." + key,
		"bad probability":        prefix + "101-35-35." + key,
		"short first padding":    prefix + "100-34-35." + key,
		"optional first padding": prefix + "50-35-35." + key,
		"reversed range":         prefix + "100-100-35." + key,
		"total padding":          prefix + "100-35-65553.0-0-0.1-1-1." + key,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateVLESSEncryption(value); err == nil {
				t.Fatal("invalid encryption accepted")
			}
		})
	}
}

func TestVLESSEncryptionBoundsTotalPaddingGap(t *testing.T) {
	key := encryptionTestX25519(t)
	for _, tc := range []struct {
		name, padding string
		valid         bool
	}{
		{"no gap", "100-35-35", true},
		{"single gap boundary", "100-35-35.100-0-60000", true},
		{"combined gap boundary", "100-35-35.100-0-30000.0-0-0.100-0-30000", true},
		{"many gaps boundary", "100-35-35" + strings.Repeat(".100-0-1000.0-0-0", 60), true},
		{"single gap over budget", "100-35-35.100-0-60001", false},
		{"combined gaps over budget", "100-35-35.100-0-30000.0-0-0.100-0-30001", false},
		{"many gaps over budget", "100-35-35" + strings.Repeat(".100-0-1000.0-0-0", 61), false},
		{"optional gaps count against budget", "100-35-35.0-0-30000.0-0-0.0-0-30001", false},
		{"day long gap", "100-35-35.100-0-86400000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encryption := "mlkem768x25519plus.native.1rtt." + tc.padding + "." + key
			err := validateVLESSEncryption(encryption)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if err != nil && !strings.Contains(err.Error(), "总间隔不能超过 60 秒") {
				t.Fatalf("unexpected error: %v", err)
			}
			raw := "vless://11111111-1111-1111-1111-111111111111@public.example.com:443?encryption=" + url.QueryEscape(encryption)
			if _, err := parseRuntimeNode(raw); (err == nil) != tc.valid {
				t.Fatalf("runtime valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestVLESSEncryptionPreservesParametersAndPublicRuntime(t *testing.T) {
	encryption := "mlkem768x25519plus.native.0rtt." + encryptionTestX25519(t)
	raw := "vless://11111111-1111-1111-1111-111111111111@public.example.com:443?security=none&type=raw&flow=xtls-rprx-vision&encryption=" + url.QueryEscape(encryption)
	pn, err := parseRuntimeNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	user := pn.Outbound["settings"].(map[string]any)["vnext"].([]any)[0].(map[string]any)["users"].([]any)[0].(map[string]any)
	if user["encryption"] != encryption || user["flow"] != "xtls-rprx-vision" {
		t.Fatal("encryption or flow was changed")
	}
	for _, raw := range []string{
		"vless://11111111-1111-1111-1111-111111111111@public.example.com:443?security=none",
		"trojan://secret@public.example.com:443?security=none",
	} {
		if _, err := parseRuntimeNode(raw); err == nil {
			t.Fatal("unencrypted public node accepted")
		}
	}
}

func TestShadowsocks2022Keys(t *testing.T) {
	key16 := base64.StdEncoding.EncodeToString(make([]byte, 16))
	key32 := base64.StdEncoding.EncodeToString(make([]byte, 32))
	cases := []struct {
		name, method, password string
		valid                  bool
	}{
		{"aes128", "2022-blake3-aes-128-gcm", key16, true},
		{"aes256", "2022-blake3-aes-256-gcm", key32, true},
		{"chacha", "2022-blake3-chacha20-poly1305", key32, true},
		{"identity chain", "2022-blake3-aes-128-gcm", key16 + ":" + key16, true},
		{"short", "2022-blake3-aes-256-gcm", key16, false},
		{"long", "2022-blake3-aes-128-gcm", key32, false},
		{"empty", "2022-blake3-aes-128-gcm", "", false},
		{"bad identity", "2022-blake3-aes-128-gcm", key16 + ":" + key32, false},
		{"empty identity", "2022-blake3-aes-128-gcm", key16 + ":", false},
		{"chacha identity", "2022-blake3-chacha20-poly1305", key32 + ":" + key32, false},
		{"unpadded", "2022-blake3-aes-128-gcm", strings.TrimRight(key16, "="), false},
		{"whitespace", "2022-blake3-aes-128-gcm", key16 + "\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "ss://" + url.PathEscape(tc.method+":"+tc.password) + "@example.com:8388"
			pn, err := parseRuntimeNode(raw)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
			if err == nil {
				server := pn.Outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
				if server["password"] != tc.password || server["method"] != tc.method {
					t.Fatal("SS2022 credentials were changed")
				}
			}
		})
	}
}

func TestEncryptionConfigsPassPinnedXray(t *testing.T) {
	key := encryptionTestX25519(t)
	for _, mode := range []string{"native", "xorpub", "random"} {
		for _, rtt := range []string{"0rtt", "1rtt"} {
			t.Run(mode+"-"+rtt, func(t *testing.T) {
				requirePinnedNodeConfig(t, "vless://11111111-1111-1111-1111-111111111111@public.example.com:443?type=raw&flow=xtls-rprx-vision&encryption=mlkem768x25519plus."+mode+"."+rtt+".100-35-100.50-0-10."+key)
			})
		}
	}
	kem, err := mlkem.GenerateKey768()
	if err != nil {
		t.Fatal(err)
	}
	t.Run("MLKEM relay", func(t *testing.T) {
		requirePinnedNodeConfig(t, "vless://11111111-1111-1111-1111-111111111111@public.example.com:443?encryption=mlkem768x25519plus.native.1rtt."+base64.RawURLEncoding.EncodeToString(kem.EncapsulationKey().Bytes())+"."+key)
	})
	for _, method := range []string{"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305"} {
		t.Run(method, func(t *testing.T) {
			size := 32
			if strings.Contains(method, "128") {
				size = 16
			}
			key := base64.StdEncoding.EncodeToString(make([]byte, size))
			requirePinnedNodeConfig(t, "ss://"+url.PathEscape(method+":"+key)+"@example.com:8388")
			if !strings.Contains(method, "chacha") {
				requirePinnedNodeConfig(t, "ss://"+url.PathEscape(method+":"+key+":"+key)+"@example.com:8388")
			}
		})
	}
}

func requirePinnedNodeConfig(t *testing.T, raw string) {
	t.Helper()
	bin := os.Getenv("PROXYSCENE_TEST_XRAY")
	if bin == "" {
		t.Skip("set PROXYSCENE_TEST_XRAY to validate against pinned Xray")
	}
	a := testApp(t)
	st := newStore()
	if _, err := a.addNode(st, raw, "synthetic-test", "default"); err != nil {
		t.Fatal(err)
	}
	st.SceneEnabled[SceneGlobal] = true
	config := filepath.Join(a.cfg.CoreDir, "node-test.json")
	if err := a.writeXrayConfigTo(st, config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, bin, "run", "-test", "-format", "json", "-config", config).CombinedOutput()
	if err != nil {
		t.Fatalf("pinned Xray rejected synthetic config: %v\n%s", err, output)
	}
}
