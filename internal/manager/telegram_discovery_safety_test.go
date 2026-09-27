package manager

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func symlinkTelegramUnitTest(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramTypedResolverDistinguishesUnitStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  telegramUnitResolutionState
		setup func(*testing.T, string)
	}{
		{"absent", telegramUnitAbsent, func(*testing.T, string) {}},
		{"regular", telegramUnitResolved, func(t *testing.T, root string) {
			writeTestUnitFile(t, filepath.Join(root, "gateway.service"), "[Service]\n")
		}},
		{"masked", telegramUnitMasked, func(t *testing.T, root string) {
			symlinkTelegramUnitTest(t, "/dev/null", filepath.Join(root, "gateway.service"))
		}},
		{"indirect mask", telegramUnitMasked, func(t *testing.T, root string) {
			symlinkTelegramUnitTest(t, "masked.service", filepath.Join(root, "gateway.service"))
			symlinkTelegramUnitTest(t, "/dev/null", filepath.Join(root, "masked.service"))
		}},
		{"dangling", telegramUnitDangling, func(t *testing.T, root string) {
			symlinkTelegramUnitTest(t, "missing.service", filepath.Join(root, "gateway.service"))
		}},
		{"named alias cycle", telegramUnitCycle, func(t *testing.T, root string) {
			symlinkTelegramUnitTest(t, "other.service", filepath.Join(root, "gateway.service"))
			symlinkTelegramUnitTest(t, "gateway.service", filepath.Join(root, "other.service"))
		}},
		{"directory", telegramUnitInvalid, func(t *testing.T, root string) {
			if err := os.Mkdir(filepath.Join(root, "gateway.service"), 0700); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", telegramUnitInvalid, func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, "gateway.service"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.setup(t, root)
			got := resolveTelegramUnitInRoots("gateway", []string{root})
			if got.State != tc.want || got.Requested != "gateway.service" {
				t.Fatalf("state=%v want=%v result=%+v", got.State, tc.want, got)
			}
			if tc.want == telegramUnitResolved || tc.want == telegramUnitAbsent || tc.want == telegramUnitMasked {
				if got.Err != nil || validateTelegramUnitResolution(got) != nil {
					t.Fatalf("stable evidence rejected: %+v", got)
				}
			} else if got.Err == nil {
				t.Fatalf("unsafe resolution has no explanatory error: %+v", got)
			}
			path, exists := effectiveUnitPathInRoots("gateway", []string{root})
			if exists != (got.State == telegramUnitResolved) || path != got.Path {
				t.Fatalf("compatibility wrapper disagrees: path=%q exists=%v result=%+v", path, exists, got)
			}
		})
	}
}

func TestTelegramTypedResolverDoesNotConfuseIOFailureWithAbsence(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	got := resolveTelegramUnitInRoots("gateway", []string{root})
	if got.State != telegramUnitIOError || !errors.Is(got.Err, syscall.ENOTDIR) {
		t.Fatalf("I/O failure treated as optional absence: %+v", got)
	}
	if got := resolveTelegramUnitInRoots("../gateway", []string{t.TempDir()}); got.State != telegramUnitInvalid || got.Err == nil {
		t.Fatalf("invalid name accepted: %+v", got)
	}
	if err := validateTelegramUnitResolution(telegramUnitResolution{}); err == nil {
		t.Fatal("empty result accepted as evidence")
	}
}

func TestTelegramTypedResolverPreservesHigherPrioritySuppression(t *testing.T) {
	for _, kind := range []string{"mask", "directory", "dangling"} {
		t.Run(kind, func(t *testing.T) {
			high, low := t.TempDir(), t.TempDir()
			canonical := "canonical.service"
			writeTestUnitFile(t, filepath.Join(low, canonical), "[Service]\n")
			symlinkTelegramUnitTest(t, filepath.Join(low, canonical), filepath.Join(high, "alias.service"))
			want := telegramUnitMasked
			switch kind {
			case "mask":
				symlinkTelegramUnitTest(t, "/dev/null", filepath.Join(high, canonical))
			case "directory":
				want = telegramUnitInvalid
				if err := os.Mkdir(filepath.Join(high, canonical), 0700); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				want = telegramUnitDangling
				symlinkTelegramUnitTest(t, "missing.service", filepath.Join(high, canonical))
			}
			got := resolveTelegramUnitInRoots("alias", []string{high, low})
			if got.State != want {
				t.Fatalf("alias bypassed higher-priority %s: %+v", kind, got)
			}
		})
	}
}

func TestTelegramTypedResolverTemplateFallbackOnlyOnAbsence(t *testing.T) {
	root := t.TempDir()
	writeTestUnitFile(t, filepath.Join(root, "gateway@.service"), "[Service]\n")
	if got := resolveTelegramUnitInRoots("gateway@first", []string{root}); got.State != telegramUnitResolved || got.Path != filepath.Join(root, "gateway@.service") {
		t.Fatalf("valid template fallback rejected: %+v", got)
	}
	symlinkTelegramUnitTest(t, "missing.service", filepath.Join(root, "gateway@broken.service"))
	if got := resolveTelegramUnitInRoots("gateway@broken", []string{root}); got.State != telegramUnitDangling {
		t.Fatalf("broken instance fell through to template: %+v", got)
	}
}

func TestTelegramTypedResolverCapturesExternalAliasChain(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	fragment := filepath.Join(external, "fragment.service")
	middle := filepath.Join(external, "middle.service")
	writeTestUnitFile(t, fragment, "[Service]\n")
	symlinkTelegramUnitTest(t, "fragment.service", middle)
	symlinkTelegramUnitTest(t, middle, filepath.Join(root, "gateway.service"))
	got := resolveTelegramUnitInRoots("gateway", []string{root})
	if got.State != telegramUnitResolved || got.Path != fragment || len(got.Chain) != 3 {
		t.Fatalf("external alias chain incomplete: %+v", got)
	}
	if err := os.Remove(middle); err != nil {
		t.Fatal(err)
	}
	symlinkTelegramUnitTest(t, "missing.service", middle)
	if err := validateTelegramUnitResolution(got); !errors.Is(err, errTelegramUnitChanged) {
		t.Fatalf("changed intermediate alias accepted: %v", err)
	}
	if result := resolveTelegramUnitInRoots("gateway", []string{root}); result.State != telegramUnitDangling {
		t.Fatalf("external dangling alias misclassified: %+v", result)
	}
}

func TestTelegramTypedResolverRejectsSameNameVendorLinkCycle(t *testing.T) {
	high, low := t.TempDir(), t.TempDir()
	symlinkTelegramUnitTest(t, filepath.Join(low, "gateway.service"), filepath.Join(high, "gateway.service"))
	symlinkTelegramUnitTest(t, filepath.Join(high, "gateway.service"), filepath.Join(low, "gateway.service"))
	if result := resolveTelegramUnitInRoots("gateway", []string{high, low}); result.State != telegramUnitCycle {
		t.Fatalf("same-name cycle not detected: %+v", result)
	}
}

func TestTelegramTypedResolverEvidenceRejectsChangedPreconditions(t *testing.T) {
	t.Run("higher-priority creation", func(t *testing.T) {
		high, low := t.TempDir(), t.TempDir()
		writeTestUnitFile(t, filepath.Join(low, "gateway.service"), "[Service]\n")
		before := resolveTelegramUnitInRoots("gateway", []string{high, low})
		writeTestUnitFile(t, filepath.Join(high, "gateway.service"), "[Service]\n")
		if err := validateTelegramUnitResolution(before); !errors.Is(err, errTelegramUnitChanged) {
			t.Fatalf("new override accepted: %v", err)
		}
	})
	t.Run("absent optional anchor appears", func(t *testing.T) {
		root := t.TempDir()
		before := resolveTelegramUnitInRoots("gateway", []string{root})
		writeTestUnitFile(t, filepath.Join(root, "gateway.service"), "[Service]\n")
		if err := validateTelegramUnitResolution(before); !errors.Is(err, errTelegramUnitChanged) {
			t.Fatalf("absent evidence reused after install: %v", err)
		}
	})
	t.Run("content replaced with same size and mtime", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "gateway.service")
		writeTestUnitFile(t, path, "[Service]\nExecStart=/bin/true\n")
		before := resolveTelegramUnitInRoots("gateway", []string{root})
		mtime := before.Chain[0].Info.ModTime()
		writeTestUnitFile(t, path, "[Service]\nExecStart=/bin/echo\n")
		if err := os.Chtimes(path, time.Now(), mtime); err != nil {
			t.Fatal(err)
		}
		if err := validateTelegramUnitResolution(before); !errors.Is(err, errTelegramUnitChanged) {
			t.Fatalf("changed bytes accepted despite restored mtime: %v", err)
		}
		if _, err := readResolvedTelegramUnitContent(before, []unitSearchRoot{{Path: root, Manage: true}}); !errors.Is(err, errTelegramUnitChanged) {
			t.Fatalf("stale resolution read accepted: %v", err)
		}
	})
	t.Run("unrelated file is irrelevant", func(t *testing.T) {
		root := t.TempDir()
		writeTestUnitFile(t, filepath.Join(root, "gateway.service"), "[Service]\n")
		before := resolveTelegramUnitInRoots("gateway", []string{root})
		writeTestUnitFile(t, filepath.Join(root, "unrelated.service"), "[Service]\n")
		if err := validateTelegramUnitResolution(before); err != nil {
			t.Fatalf("unrelated entry invalidated exact resolution: %v", err)
		}
	})
}

func TestTelegramDiscoveryRejectsMalformedClassification(t *testing.T) {
	cases := map[string]string{
		"Environment":                       "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\nEnvironment=\"SECRET-unterminated\nExecStart=/usr/bin/true\n",
		"UnsetEnvironment":                  "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\nUnsetEnvironment=\"SECRET-unterminated\n",
		"ExecStart":                         "[Service]\nExecStart=/usr/bin/python -m hermes_cli.main gateway run \"SECRET-unterminated\n",
		"OpenClaw with malformed ExecStart": "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\nExecStart=\"SECRET-unterminated\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := classifyTelegramUnitContent(content); err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("classification error missing or leaking values: %v", err)
			}
			root := t.TempDir()
			path := filepath.Join(root, "previously-managed.service")
			writeTestUnitFile(t, path, content)
			if selected, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: root, Manage: true}}, ""); err == nil || len(selected) != 0 {
				t.Fatalf("malformed unit became trusted exclusion: selected=%v err=%v", selected, err)
			}
			if related, err := telegramRelatedUnit(path, "previously-managed.service"); related || err == nil {
				t.Fatalf("file classification swallowed error: related=%v err=%v", related, err)
			}
		})
	}
}

func TestTelegramDiscoveryKeepsDanglingAliasFailureExplicit(t *testing.T) {
	root := t.TempDir()
	writeTestUnitFile(t, filepath.Join(root, "hermes-gateway.service"), "[Service]\nExecStart=/usr/bin/python -m hermes_cli.main gateway run\n")
	symlinkTelegramUnitTest(t, "missing-timesync.service", filepath.Join(root, "dbus-org.freedesktop.timesync1.service"))
	selected, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: root, Manage: true}}, "")
	if err == nil || !strings.Contains(err.Error(), "别名目标不存在") {
		t.Fatalf("unproven unrelated alias silently accepted: selected=%v err=%v", selected, err)
	}
}

func TestTelegramDropInDiscoveryRejectsUnresolvedAlias(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gateway.service")
	writeTestUnitFile(t, path, "[Service]\n")
	symlinkTelegramUnitTest(t, "missing.service", filepath.Join(root, "broken-alias.service"))
	if _, err := readTelegramUnitContentWithDropIns(path, "gateway.service", []unitSearchRoot{{Path: root, Manage: true}}); err == nil {
		t.Fatal("alias enumeration silently omitted unresolved alias")
	}
}

func TestTelegramDiscoveryRespectsCanonicalMaskThroughAlias(t *testing.T) {
	high, low := t.TempDir(), t.TempDir()
	writeTestUnitFile(t, filepath.Join(low, "gateway.service"), "[Service]\nEnvironment=OPENCLAW_SERVICE_MARKER=openclaw OPENCLAW_SERVICE_KIND=gateway\n")
	symlinkTelegramUnitTest(t, "/dev/null", filepath.Join(high, "gateway.service"))
	symlinkTelegramUnitTest(t, filepath.Join(low, "gateway.service"), filepath.Join(high, "alias.service"))
	if selected, err := discoverEffectiveTelegramUnits([]unitSearchRoot{{Path: high, Manage: true}, {Path: low, Manage: true}}, ""); err != nil || len(selected) != 0 {
		t.Fatalf("canonical mask not honored through alias: selected=%v err=%v", selected, err)
	}
}

func TestTelegramTypedResolverEmptyUnitMasksVendorFallback(t *testing.T) {
	high, low := t.TempDir(), t.TempDir()
	writeTestUnitFile(t, filepath.Join(high, "gateway.service"), "")
	writeTestUnitFile(t, filepath.Join(low, "gateway.service"), "[Service]\n")
	got := resolveTelegramUnitInRoots("gateway", []string{high, low})
	if got.State != telegramUnitMasked || got.Err != nil {
		t.Fatalf("empty unit did not mask vendor: %+v", got)
	}
}

func TestTelegramTypedResolverSupportsLinkedParentDirectory(t *testing.T) {
	root, external := t.TempDir(), t.TempDir()
	realDir := filepath.Join(external, "real")
	fragment := filepath.Join(realDir, "gateway.service")
	writeTestUnitFile(t, fragment, "[Service]\nExecStart=/usr/bin/python -m hermes_cli.main gateway run\n")
	symlinkTelegramUnitTest(t, realDir, filepath.Join(external, "linked"))
	symlinkTelegramUnitTest(t, filepath.Join(external, "linked", "gateway.service"), filepath.Join(root, "gateway.service"))
	got := resolveTelegramUnitInRoots("gateway", []string{root})
	if got.State != telegramUnitResolved || got.Path != fragment {
		t.Fatalf("linked parent did not resolve: %+v", got)
	}
	content, err := readResolvedTelegramUnitContent(got, []unitSearchRoot{{Path: root, Manage: true}})
	if err != nil || !unitIsHermesGatewayExec(content) {
		t.Fatalf("linked unit read failed: %v", err)
	}
}

func TestTelegramTypedResolverBoundsAliasChains(t *testing.T) {
	for _, count := range []int{maxTelegramUnitAliasDepth, maxTelegramUnitAliasDepth + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			root := t.TempDir()
			for i := 0; i < count; i++ {
				symlinkTelegramUnitTest(t, fmt.Sprintf("gateway-%d.service", i+1), filepath.Join(root, fmt.Sprintf("gateway-%d.service", i)))
			}
			writeTestUnitFile(t, filepath.Join(root, fmt.Sprintf("gateway-%d.service", count)), "[Service]\n")
			got := resolveTelegramUnitInRoots("gateway-0", []string{root})
			want := telegramUnitResolved
			if count > maxTelegramUnitAliasDepth {
				want = telegramUnitCycle
			}
			if got.State != want {
				t.Fatalf("alias depth=%d state=%v want=%v err=%v", count, got.State, want, got.Err)
			}
		})
	}
}
