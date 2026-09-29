package manager

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	updateBundleMaxFile  int64 = 256 << 20
	updateBundleMaxTotal int64 = 512 << 20
)

var updateBundleFiles = [...]string{
	"LICENSE", "LICENSE-Xray", "NOTICE", "SOURCE-Xray", "THIRD_PARTY_LICENSES",
	"THIRD_PARTY_LICENSES-Xray", "install.sh", "proxyscene", "xray", "xray-version.txt",
}

// Counting the raw tar bytes also rejects extension headers that archive/tar
// would otherwise silently consume (PAX, GNU long names and sparse metadata).
type updateTarReader struct {
	io.Reader
	n int64
}

func (r *updateTarReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += int64(n)
	return n, err
}

// extractUpdateBundle accepts only the release bundle's flat, exact layout.
// The caller must first verify the archive against the selected release source's checksum
// and supply a nonexistent destination in a trusted private staging directory.
func extractUpdateBundle(archive, destination, arch string) (bundleDir string, err error) {
	if _, _, err := updateELFArchitecture(arch); err != nil {
		return "", err
	}
	if !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return "", errors.New("升级解包目录必须是规范化绝对路径")
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		return "", fmt.Errorf("创建专用升级解包目录：%w", err)
	}
	defer func() {
		if err != nil {
			if cleanupErr := os.RemoveAll(destination); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("清理升级解包目录：%w", cleanupErr))
			}
		}
	}()

	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer f.Close()
	compressed := bufio.NewReader(f)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return "", fmt.Errorf("读取升级 gzip：%w", err)
	}
	defer gz.Close()
	gz.Multistream(false)
	// Payload limits alone do not bound hidden tar metadata or trailing padding.
	raw := &updateTarReader{Reader: io.LimitReader(gz, updateBundleMaxTotal+(1<<20)+1)}
	tr := tar.NewReader(raw)
	root := "proxyscene_bundle_linux_" + arch + "/"
	allowed := map[string]bool{"bundle-manifest.sha256": true}
	for _, name := range updateBundleFiles {
		allowed[name] = true
	}
	seen := make(map[string]bool, len(allowed))
	rootSeen := false
	var total, nextHeader int64
	for {
		h, nextErr := tr.Next()
		if nextErr == io.EOF {
			if raw.n != nextHeader+1024 {
				return "", errors.New("升级 tar 缺少规范结束标记")
			}
			break
		}
		if nextErr != nil {
			return "", fmt.Errorf("读取升级 tar：%w", nextErr)
		}
		if raw.n != nextHeader+512 || len(h.PAXRecords) != 0 || h.Linkname != "" {
			return "", errors.New("升级 tar 不允许扩展元数据或链接")
		}
		if h.Size < 0 || h.Size > updateBundleMaxFile || total > updateBundleMaxTotal-h.Size {
			return "", errors.New("升级 bundle 超出解压大小限制")
		}
		nextHeader = raw.n + h.Size + ((-h.Size) & 511)
		if h.Name == root && h.Typeflag == tar.TypeDir && !rootSeen && h.Size == 0 {
			bundleDir = filepath.Join(destination, strings.TrimSuffix(root, "/"))
			if err := os.Mkdir(bundleDir, 0o700); err != nil {
				return "", err
			}
			rootSeen = true
			continue
		}
		name, inRoot := strings.CutPrefix(h.Name, root)
		if !rootSeen || !inRoot || !allowed[name] || seen[name] || h.Typeflag != tar.TypeReg {
			return "", fmt.Errorf("升级 bundle 含有非预期、重复或非普通文件：%q", h.Name)
		}
		out, err := os.OpenFile(filepath.Join(bundleDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return "", err
		}
		_, copyErr := io.CopyN(out, tr, h.Size)
		closeErr := out.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return "", fmt.Errorf("解包 %s：%w", name, err)
		}
		seen[name] = true
		total += h.Size
	}
	if !rootSeen || len(seen) != len(allowed) {
		return "", errors.New("升级 bundle 缺少必需文件")
	}
	// GNU tar pads its final record with zeros. Read through the gzip trailer
	// to check its CRC, but never accept another tar archive or gzip member.
	padding, err := io.ReadAll(io.LimitReader(raw, 10241))
	if err != nil {
		return "", fmt.Errorf("校验升级 gzip 尾部：%w", err)
	}
	if len(padding) > 10240 || strings.Trim(string(padding), "\x00") != "" {
		return "", errors.New("升级 tar 结束后有额外内容")
	}
	if _, err := compressed.ReadByte(); err != io.EOF {
		return "", errors.New("升级 gzip 结束后有额外数据或拼接归档")
	}
	if err := validateUpdateBundleManifest(bundleDir); err != nil {
		return "", err
	}
	return bundleDir, nil
}

func validateUpdateBundleManifest(bundleDir string) error {
	f, err := os.Open(filepath.Join(bundleDir, "bundle-manifest.sha256"))
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, 8193))
	closeErr := f.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return err
	}
	if len(data) > 8192 || !strings.HasSuffix(string(data), "\n") {
		return errors.New("升级 bundle manifest 大小或格式错误")
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != len(updateBundleFiles) {
		return errors.New("升级 bundle manifest 条目数错误")
	}
	allowed := make(map[string]bool, len(updateBundleFiles))
	for _, name := range updateBundleFiles {
		allowed[name] = true
	}
	for _, line := range lines {
		if len(line) < 67 || line[64:66] != "  " || !allowed[line[66:]] {
			return errors.New("升级 bundle manifest 含有非预期或重复条目")
		}
		expected, err := hex.DecodeString(line[:64])
		if err != nil || hex.EncodeToString(expected) != line[:64] {
			return errors.New("升级 bundle manifest SHA256 格式错误")
		}
		name := line[66:]
		file, err := os.Open(filepath.Join(bundleDir, name))
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != line[:64] {
			return fmt.Errorf("升级 bundle 组件 SHA256 不匹配：%s", name)
		}
		delete(allowed, name)
	}
	return nil
}

func updateELFArchitecture(arch string) (elf.Class, elf.Machine, error) {
	switch arch {
	case "amd64":
		return elf.ELFCLASS64, elf.EM_X86_64, nil
	case "arm64":
		return elf.ELFCLASS64, elf.EM_AARCH64, nil
	case "386":
		return elf.ELFCLASS32, elf.EM_386, nil
	case "armv7":
		return elf.ELFCLASS32, elf.EM_ARM, nil
	default:
		return 0, 0, fmt.Errorf("不支持的升级架构：%s", arch)
	}
}

func validateUpdateELF(path, arch string) error {
	class, machine, err := updateELFArchitecture(arch)
	if err != nil {
		return err
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("读取升级 ELF：%w", err)
	}
	defer f.Close()
	if f.Class != class || f.Machine != machine || f.Data != elf.ELFDATA2LSB || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return fmt.Errorf("升级 ELF 架构或可执行类型不匹配：%s", filepath.Base(path))
	}
	return nil
}

func validateUpdateBundleIdentity(bundleDir, arch string) error {
	if err := validateUpdateManagerIdentity(filepath.Join(bundleDir, "proxyscene"), arch); err != nil {
		return err
	}
	return validateUpdateELF(filepath.Join(bundleDir, "xray"), arch)
}

// Validate build metadata without executing the downloaded program. The selected release source's
// verified archive checksum binds its contents to the release tag and commit.
// Release builds use -trimpath, which intentionally omits -ldflags from build
// info: this check cannot independently recover the linked Version or Commit.
func validateUpdateManagerIdentity(path, arch string) error {
	if err := validateUpdateELF(path, arch); err != nil {
		return err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取升级 Go 构建身份：%w", err)
	}
	if info.Main.Path != "proxyscene" || info.Path != "proxyscene/cmd/proxyscene" {
		return errors.New("升级程序的 Go 模块身份不匹配")
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		if _, exists := settings[setting.Key]; exists {
			return errors.New("升级程序含有重复 Go 构建设置")
		}
		settings[setting.Key] = setting.Value
	}
	goarch := arch
	if arch == "armv7" {
		goarch = "arm"
		if settings["GOARM"] != "7" && settings["GOARM"] != "7,hardfloat" {
			return errors.New("升级程序的 GOARM 不匹配")
		}
	}
	if settings["GOOS"] != "linux" || settings["GOARCH"] != goarch {
		return errors.New("升级程序的 Go 平台不匹配")
	}
	return nil
}

func updateInstallerEnv(cfg Config) []string {
	return []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
		"PROXYSCENE_MANAGER_DIR=" + cfg.CoreDir,
		"PROXYSCENE_SWITCH_BIN=" + cfg.InstallBin,
		"PROXYSCENE_SYSTEMD_SERVICE_NAME=" + cfg.SystemdService,
		"PROXYSCENE_BOOT_RESTORE_SERVICE_NAME=" + cfg.RestoreService,
	}
}

func runUpdateInstaller(bundleDir string, cfg Config, expectedSHA256 string) error {
	const bash = "/usr/bin/bash"
	if err := cfg.ValidateLocators(); err != nil {
		return err
	}
	digest, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != expectedSHA256 {
		return errors.New("升级前程序 SHA256 格式错误")
	}
	if err := validatePrivilegedExecutable(bash, "升级安装器 bash"); err != nil {
		return err
	}
	cmd := exec.Command(bash, "--noprofile", "--norc", filepath.Join(bundleDir, "install.sh"), "--offline", "--expected-manager-sha256", expectedSHA256)
	cmd.Dir = bundleDir
	cmd.Env = updateInstallerEnv(cfg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("离线升级安装器执行失败：%w", err)
	}
	return nil
}
