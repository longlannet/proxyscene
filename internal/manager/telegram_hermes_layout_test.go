package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestHermesProjectRootUsesOnlyDirectExecutable(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		root string
	}{
		{"python", []string{"/root/.hermes/hermes-agent/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"}, "/root/.hermes/hermes-agent"},
		{"system python", []string{"/usr/local/lib/hermes-agent/venv/bin/python3.11", "-m", "hermes_cli", "gateway", "run"}, hermesSystemProjectRoot},
		{"allowed prefixes", []string{"-:/usr/local/lib/hermes-agent/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"}, hermesSystemProjectRoot},
		{"console script", []string{"/usr/local/lib/hermes-agent/venv/bin/hermes-agent", "gateway", "run"}, hermesSystemProjectRoot},
		{"console alias", []string{"/usr/local/lib/hermes-agent/venv/bin/hermes_cli", "gateway", "run"}, hermesSystemProjectRoot},
		{"earlier marker", []string{"/srv/venv/bin/project/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"}, "/srv/venv/bin/project"},
		{"option cannot supply root", []string{"/usr/bin/python", "-m", "hermes_cli.main", "gateway", "run", "/usr/local/lib/hermes-agent/venv/bin/python"}, ""},
		{"relative executable", []string{"python", "-m", "hermes_cli.main", "gateway", "run", "/usr/local/lib/hermes-agent/venv/bin/python"}, ""},
		{"noncanonical executable", []string{"/usr/local/lib/hermes-agent/venv/../venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"}, ""},
		{"nested bin directory", []string{"/usr/local/lib/hermes-agent/venv/bin/other/python", "-m", "hermes_cli.main", "gateway", "run"}, ""},
		{"root project", []string{"/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"}, ""},
		{"wrapper", []string{"/usr/bin/env", "/usr/local/lib/hermes-agent/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"}, ""},
		{"credential prefix", []string{"+/usr/local/lib/hermes-agent/venv/bin/python", "-m", "hermes_cli.main", "gateway", "run"}, ""},
		{"not gateway", []string{"/usr/local/lib/hermes-agent/venv/bin/python", "-m", "other", "gateway", "run"}, ""},
		{"empty", nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := hermesProjectRootFromArgv(test.argv)
			if (err == nil) != (test.root != "") || got != test.root {
				t.Fatalf("root=%q err=%v; want %q", got, err, test.root)
			}
		})
	}
}

func TestHermesProjectLayoutsKeepConfigurationRootIndependent(t *testing.T) {
	for _, test := range []struct {
		home, project string
		system, valid bool
	}{
		{"/root/.hermes", "/root/.hermes/hermes-agent", false, true},
		{"/home/alice/.hermes", "/home/alice/.hermes/hermes-agent", false, true},
		{"/home/alice/hermes-data", "/home/alice/hermes-data/hermes-agent", false, true},
		{"/root/.hermes", hermesSystemProjectRoot, true, true},
		{"/home/alice/.hermes", hermesSystemProjectRoot, true, true},
		{"/home/alice/custom", hermesSystemProjectRoot, true, true},
		{"/usr/local/lib", hermesSystemProjectRoot, true, true},
		{"/home/alice/.hermes", "/root/.hermes/hermes-agent", false, false},
		{"/home/alice/.hermes", "/opt/hermes-agent", false, false},
		{"/root/.hermes", hermesSystemProjectRoot + "/other", false, false},
		{"/root/.hermes", "/usr/local/lib/../lib/hermes-agent", false, false},
	} {
		t.Run(test.home+"_"+test.project, func(t *testing.T) {
			got, err := hermesProjectUsesSystemLayout(test.home, test.project)
			if (err == nil) != test.valid || got != test.system {
				t.Fatalf("system=%v err=%v; want system=%v valid=%v", got, err, test.system, test.valid)
			}
		})
	}
}

func TestHermesUserProjectMustExistWithoutDotenv(t *testing.T) {
	user, identity := contractTestIdentity(t)
	root := filepath.Join(identity.Home, ".hermes", "hermes-agent")
	if err := validateHermesUserProjectDirectory(user, identity, root); err == nil {
		t.Fatal("missing project treated as an optional missing .env")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateHermesUserProjectDirectory(user, identity, root); err != nil {
		t.Fatalf("existing project without .env rejected: %v", err)
	}
	if err := rejectHermesRuntimeDotEnv(user, identity, filepath.Join(root, ".env")); err != nil {
		t.Fatalf("missing optional project .env rejected: %v", err)
	}
	if err := os.Rename(root, root+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root+".real", root); err != nil {
		t.Fatal(err)
	}
	if err := validateHermesUserProjectDirectory(user, identity, root); err == nil {
		t.Fatal("project directory symlink accepted")
	}
}

func hermesSystemLayoutFixture(t *testing.T) (string, func(string) error, func(string) error) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("system installation fixture must be root-owned")
	}
	anchor := t.TempDir()
	project := filepath.Join(anchor, "hermes-agent")
	if err := os.MkdirAll(filepath.Join(project, "venv", "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"python", "hermes-agent", "hermes_cli"} {
		if err := os.WriteFile(filepath.Join(project, "venv", "bin", name), []byte("fixture executable; never run\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Only the private test helper receives this anchor. The public runtime path
	// uses a fixed system installation and validates ancestors through /.
	check := func(path string) error {
		if path != anchor && !strings.HasPrefix(path, anchor+string(os.PathSeparator)) {
			return fmt.Errorf("fixture path escaped anchor: %s", path)
		}
		for current := path; ; current = filepath.Dir(current) {
			if err := validateHermesSystemNode(current, true); err != nil {
				return err
			}
			if current == anchor {
				return nil
			}
		}
	}
	return project, check, func(path string) error {
		return validateHermesSystemExecutableAt(anchor, path)
	}
}

func hermesSystemLayoutUnit(project, entry string, hooks bool) string {
	command := filepath.Join(project, "venv", "bin", entry)
	args := " gateway run"
	if entry == "python" {
		args = " -m hermes_cli.main gateway run"
	}
	unit := "[Service]\nExecStart=" + command + args + "\n"
	if hooks {
		python := filepath.Join(project, "venv", "bin", "python")
		unit += "ExecStop=-" + python + " -m gateway.systemd_stop_mark\nExecStopPost=-" + python + " -m gateway.cgroup_cleanup\n"
	}
	return unit
}

func TestHermesSystemProjectAllowsOfficialExecutableForms(t *testing.T) {
	for _, entry := range []string{"python", "hermes-agent", "hermes_cli"} {
		t.Run(entry, func(t *testing.T) {
			project, check, checkExecutable := hermesSystemLayoutFixture(t)
			python := filepath.Join(project, "venv", "bin", "python")
			if err := os.Rename(python, python+"3.11"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("python3.11", python+"3"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("python3", python); err != nil {
				t.Fatal(err)
			}
			unit := hermesSystemLayoutUnit(project, entry, true)
			if _, err := validateHermesEffectiveUnit(unit); err != nil {
				t.Fatal(err)
			}
			if err := validateHermesSystemProjectWithChecks(unit, project, check, checkExecutable); err != nil {
				t.Fatalf("normal venv leaf links rejected: %v", err)
			}
		})
	}
}

func TestHermesSystemProjectRejectsUnsafeInstallations(t *testing.T) {
	for _, kind := range []string{"missing root", "missing venv", "missing bin", "root writable", "venv writable", "bin writable", "foreign root", "root symlink", "venv symlink", "bin symlink", "missing executable", "foreign executable", "writable executable", "non executable", "executable fifo", "dangling link", "link cycle", "foreign link", "noncanonical link target", "writable link target parent", "console unsafe stop interpreter"} {
		t.Run(kind, func(t *testing.T) {
			project, check, checkExecutable := hermesSystemLayoutFixture(t)
			venv := filepath.Join(project, "venv")
			bin := filepath.Join(venv, "bin")
			python := filepath.Join(bin, "python")
			entry := "python"
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "missing root":
				must(os.RemoveAll(project))
			case "missing venv":
				must(os.RemoveAll(venv))
			case "missing bin":
				must(os.RemoveAll(bin))
			case "root writable":
				must(os.Chmod(project, 0777))
			case "venv writable":
				must(os.Chmod(venv, 0775))
			case "bin writable":
				must(os.Chmod(bin, 0757))
			case "foreign root":
				must(os.Chown(project, 1, 1))
			case "root symlink", "venv symlink", "bin symlink":
				path := project
				if kind == "venv symlink" {
					path = venv
				} else if kind == "bin symlink" {
					path = bin
				}
				must(os.Rename(path, path+".real"))
				must(os.Symlink(path+".real", path))
			case "missing executable":
				must(os.Remove(python))
			case "foreign executable":
				must(os.Chown(python, 1, 1))
			case "writable executable":
				must(os.Chmod(python, 0775))
			case "non executable":
				must(os.Chmod(python, 0644))
			case "executable fifo":
				must(os.Remove(python))
				must(syscall.Mkfifo(python, 0755))
			case "dangling link", "link cycle", "foreign link":
				must(os.Remove(python))
				target := "missing-python"
				if kind == "link cycle" {
					target = "python"
				}
				must(os.Symlink(target, python))
				if kind == "foreign link" {
					must(os.Lchown(python, 1, 1))
				}
			case "noncanonical link target":
				must(os.Rename(python, python+"3"))
				must(os.Symlink("unverified/../python3", python))
			case "writable link target parent":
				other := filepath.Join(project, "interpreter")
				must(os.Mkdir(other, 0755))
				must(os.Rename(python, filepath.Join(other, "python")))
				must(os.Chmod(other, 0777))
				must(os.Symlink(filepath.Join(other, "python"), python))
			case "console unsafe stop interpreter":
				entry = "hermes-agent"
				must(os.Chmod(python, 0777))
			}
			unit := hermesSystemLayoutUnit(project, entry, true)
			if err := validateHermesSystemProjectWithChecks(unit, project, check, checkExecutable); err == nil {
				t.Fatal("unsafe system installation accepted")
			}
		})
	}
}

func TestHermesSystemDirectoryRejectsWritableAncestor(t *testing.T) {
	project, _, _ := hermesSystemLayoutFixture(t)
	if err := validateHermesSystemDirectory(project); err == nil {
		t.Fatal("production validator accepted installation under writable /tmp")
	}
}

func TestHermesSystemProjectAllowsTrustedUVDirectoryLinks(t *testing.T) {
	for _, kind := range []string{"absolute", "relative", "relative parent", "chained"} {
		t.Run(kind, func(t *testing.T) {
			project, check, checkExecutable := hermesSystemLayoutFixture(t)
			python := filepath.Join(project, "venv", "bin", "python")
			generation := filepath.Join(project, ".hermes-runtime", "python", "generation")
			version := filepath.Join(generation, "cpython-3.11.15-linux-x86_64-gnu")
			alias := filepath.Join(generation, "cpython-3.11-linux-x86_64-gnu")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.MkdirAll(filepath.Join(version, "bin"), 0755))
			must(os.Rename(python, filepath.Join(version, "bin", "python3.11")))
			directoryTarget := version
			pythonTarget := filepath.Join(alias, "bin", "python3.11")
			switch kind {
			case "relative":
				directoryTarget = filepath.Base(version)
				pythonTarget = "../../.hermes-runtime/python/generation/cpython-3.11-linux-x86_64-gnu/bin/python3.11"
			case "relative parent":
				directoryTarget = "../generation/" + filepath.Base(version)
			case "chained":
				must(os.Symlink(version, alias+"-current"))
				directoryTarget = filepath.Base(alias) + "-current"
				must(os.Symlink("python3.11", filepath.Join(version, "bin", "python3")))
				pythonTarget = filepath.Join(alias, "bin", "python3")
			}
			must(os.Symlink(directoryTarget, alias))
			must(os.Symlink(pythonTarget, python))
			if err := check(filepath.Join(alias, "bin")); err == nil {
				t.Fatal("strict installation directory checker accepted the uv alias")
			}
			for _, entry := range []string{"python", "hermes-agent"} {
				unit := hermesSystemLayoutUnit(project, entry, true)
				if err := validateHermesSystemProjectWithChecks(unit, project, check, checkExecutable); err != nil {
					t.Fatalf("trusted uv runtime rejected for %s: %v", entry, err)
				}
			}
		})
	}
}

func TestHermesSystemExecutableRejectsUnsafeDirectoryLinks(t *testing.T) {
	for _, kind := range []string{
		"foreign alias", "writable alias parent", "foreign alias parent", "writable target parent", "foreign target parent",
		"writable target", "foreign target", "writable target bin", "foreign target bin", "writable binary", "foreign binary",
		"directory cycle", "dangling directory", "directory target is file", "noncanonical directory target", "noncanonical binary target",
		"absolute escape", "relative escape", "binary is directory", "binary is fifo", "relative executable", "noncanonical executable",
	} {
		t.Run(kind, func(t *testing.T) {
			project, _, checkExecutable := hermesSystemLayoutFixture(t)
			python := filepath.Join(project, "venv", "bin", "python")
			aliasParent := filepath.Join(project, "runtime")
			targetParent := filepath.Join(project, "store")
			version := filepath.Join(targetParent, "cpython-3.11.15")
			alias := filepath.Join(aliasParent, "cpython-3.11")
			binary := filepath.Join(version, "bin", "python3.11")
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(os.MkdirAll(aliasParent, 0755))
			must(os.MkdirAll(filepath.Dir(binary), 0755))
			must(os.Rename(python, binary))
			aliasTarget := version
			pythonTarget := filepath.Join(alias, "bin", "python3.11")
			mutations := map[string]string{
				"alias parent": aliasParent, "target parent": targetParent, "target": version,
				"target bin": filepath.Dir(binary), "binary": binary,
			}
			for name, path := range mutations {
				if kind == "writable "+name {
					must(os.Chmod(path, 0775))
				} else if kind == "foreign "+name {
					must(os.Chown(path, 1, 1))
				}
			}
			switch kind {
			case "directory cycle":
				aliasTarget = filepath.Base(alias)
			case "dangling directory":
				aliasTarget = "missing"
			case "directory target is file":
				aliasTarget = binary
			case "noncanonical directory target":
				aliasTarget = targetParent + "/unchecked/../" + filepath.Base(version)
			case "noncanonical binary target":
				pythonTarget = alias + "/bin/../bin/python3.11"
			case "absolute escape":
				aliasTarget = "/usr/bin"
			case "relative escape":
				aliasTarget = "../../../usr/bin"
			case "binary is directory":
				must(os.Remove(binary))
				must(os.Mkdir(binary, 0755))
			case "binary is fifo":
				must(os.Remove(binary))
				must(syscall.Mkfifo(binary, 0755))
			}
			must(os.Symlink(aliasTarget, alias))
			must(os.Symlink(pythonTarget, python))
			if kind == "foreign alias" {
				must(os.Lchown(alias, 1, 1))
			}
			if kind == "relative executable" {
				python = "venv/bin/python"
			} else if kind == "noncanonical executable" {
				python = project + "/venv/../venv/bin/python"
			}
			if err := checkExecutable(python); err == nil {
				t.Fatal("unsafe interpreter path accepted")
			}
		})
	}
}

func TestHermesSystemExecutableBoundsCombinedDirectoryAndLeafLinks(t *testing.T) {
	for _, count := range []int{16, 17} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			project, _, checkExecutable := hermesSystemLayoutFixture(t)
			python := filepath.Join(project, "venv", "bin", "python")
			binary := python + "3.11"
			if err := os.Rename(python, binary); err != nil {
				t.Fatal(err)
			}
			for i := count - 2; i >= 0; i-- {
				target := filepath.Join(project, "venv", "bin")
				if i < count-2 {
					target = filepath.Join(project, fmt.Sprintf("alias-%d", i+1))
				}
				if err := os.Symlink(target, filepath.Join(project, fmt.Sprintf("alias-%d", i))); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(filepath.Join(project, "alias-0", filepath.Base(binary)), python); err != nil {
				t.Fatal(err)
			}
			err := checkExecutable(python)
			if (err == nil) != (count == 16) {
				t.Fatalf("combined links=%d: %v", count, err)
			}
		})
	}
}
