package manager

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The snapshot comes from the same pinned descriptor as the configuration
// bytes. Device/inode bind a quarantined file to that snapshot; owner, group
// and permissions also participate in CAS, including interrupted writes.
type userFileMetadata struct {
	UID, GID int
	Mode     os.FileMode
	Device   uint64
	Inode    uint64
}

// Replacing an ACL-bearing file using only chmod would silently change its
// access policy. Until we can preserve every ACL model, refuse these files.
var userFileACLNames = [...]string{"system.posix_acl_access", "system.nfs4_acl"}

func noUserFileXattr(err error) bool {
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP)
}

func rejectUserFileACL(fd int) error {
	for _, name := range userFileACLNames {
		_, err := unix.Fgetxattr(fd, name, nil)
		if err == nil {
			return fmt.Errorf("用户配置含扩展 ACL，拒绝修改以保留访问权限")
		}
		if !noUserFileXattr(err) {
			return fmt.Errorf("无法验证用户配置 ACL：%w", err)
		}
	}
	return nil
}

func clearInheritedUserFileACL(fd int) error {
	// A 0600 creation masks inherited named POSIX grants. Delete that inherited
	// ACL before writing, because a later chmod could otherwise activate them.
	if err := unix.Fremovexattr(fd, "system.posix_acl_access"); err != nil && !noUserFileXattr(err) {
		return fmt.Errorf("无法清除用户配置临时文件的继承 ACL：%w", err)
	}
	return rejectUserFileACL(fd)
}

func readUserFileMetadata(fd int) (*userFileMetadata, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("用户配置必须是非符号链接普通文件")
	}
	if st.Mode&0o7000 != 0 {
		return nil, fmt.Errorf("用户配置含特殊权限位，拒绝修改以保留访问权限")
	}
	if st.Size > maxDevConfigBytes {
		return nil, fmt.Errorf("用户配置超过大小限制")
	}
	if err := rejectUserFileACL(fd); err != nil {
		return nil, err
	}
	return &userFileMetadata{UID: int(st.Uid), GID: int(st.Gid), Mode: os.FileMode(st.Mode & 0o777), Device: uint64(st.Dev), Inode: st.Ino}, nil
}

func (m *userFileMetadata) verifyFD(fd int, sameInode bool) error {
	current, err := readUserFileMetadata(fd)
	if err != nil {
		return errors.Join(errUserFileChanged, err)
	}
	if m.UID != current.UID || m.GID != current.GID || m.Mode != current.Mode || sameInode && (m.Device != current.Device || m.Inode != current.Inode) {
		return errors.Join(errUserFileChanged, fmt.Errorf("用户配置属主、属组、权限或文件身份已变化"))
	}
	return nil
}

func readUserFileWithExpectedMetadata(dirFD int, name string, max int64, metadata *userFileMetadata, sameInode bool) ([]byte, error) {
	if metadata == nil {
		return readRegularFileAtNoFollow(dirFD, name, max)
	}
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	if err := metadata.verifyFD(fd, sameInode); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("文件超过大小限制 %d 字节", max)
	}
	if err := metadata.verifyFD(fd, sameInode); err != nil {
		return nil, err
	}
	return data, nil
}

func writeUserFileAtomicCASMetadataPersisted(userName string, expectedIdentity *persistedUserIdentity, lookup localUserIdentityLookup, path string, expected, data []byte, metadata *userFileMetadata) error {
	identity, err := verifyPersistedUserIdentity(userName, expectedIdentity, lookup)
	if err != nil {
		return err
	}
	if metadata == nil || metadata.UID != identity.UID {
		return fmt.Errorf("用户配置元数据与记录身份不一致，拒绝修改")
	}
	return writeUserFileAtomicCASWithMetadata(userName, identity, path, expected, data, metadata.Mode, metadata)
}

// A missing final name can be the middle of an interrupted CAS. Without its
// complete snapshot we cannot safely reconstruct it or discard its contents.
func ensureDevConfigNoQuarantine(dirFD int) error {
	var st unix.Stat_t
	err := unix.Fstatat(dirFD, userFileQuarantineName, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法验证开发配置遗留隔离文件：%w", err)
	}
	return errors.Join(errUserFileChanged, fmt.Errorf("开发配置隔离文件尚未恢复，已保留并拒绝创建新配置"))
}

func writeDevConfigCreatePersisted(user string, expectedIdentity *persistedUserIdentity, path string, data []byte) error {
	identity, err := verifyPersistedUserIdentity(user, expectedIdentity, devLookupUserIdentity)
	if err != nil {
		return err
	}
	clean, dirFD, err := openUserFileDirForIdentity(user, identity, path, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	check := func() error { return ensureDevConfigNoQuarantine(dirFD) }
	if err := check(); err != nil {
		return err
	}
	err = writeUserFileAtomicAtModeChecked(dirFD, filepath.Base(clean), data, 0o600, identity.UID, identity.GID, true, check)
	if errors.Is(err, unix.EEXIST) {
		return errUserFileChanged
	}
	return err
}
