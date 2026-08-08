package manager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type persistedUserIdentity struct {
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Home string `json:"home"`
}

type localUserIdentityLookup func(string) (localUserIdentity, error)

func capturePersistedUserIdentity(userName string, lookup localUserIdentityLookup) (*persistedUserIdentity, error) {
	identity, err := lookup(userName)
	if err != nil {
		return nil, err
	}
	persisted := &persistedUserIdentity{
		UID:  identity.UID,
		GID:  identity.GID,
		Home: filepath.Clean(identity.Home),
	}
	if err := validatePersistedUserIdentity(userName, persisted); err != nil {
		return nil, err
	}
	return persisted, nil
}

func validatePersistedUserIdentity(userName string, identity *persistedUserIdentity) error {
	if identity == nil {
		return fmt.Errorf("用户 %s 的 ownership 记录缺少 uid/gid/home 身份绑定，拒绝自动操作", userName)
	}
	if identity.UID < 0 || identity.GID < 0 {
		return fmt.Errorf("用户 %s 的 ownership 身份 uid/gid 无效", userName)
	}
	if strings.TrimSpace(identity.Home) == "" || strings.IndexByte(identity.Home, 0) >= 0 ||
		!filepath.IsAbs(identity.Home) || filepath.Clean(identity.Home) != identity.Home ||
		identity.Home == string(os.PathSeparator) {
		return fmt.Errorf("用户 %s 的 ownership 身份 home 无效：%s", userName, identity.Home)
	}
	return nil
}

func verifyPersistedUserIdentity(userName string, expected *persistedUserIdentity, lookup localUserIdentityLookup) (localUserIdentity, error) {
	if err := validatePersistedUserIdentity(userName, expected); err != nil {
		return localUserIdentity{}, err
	}
	current, err := lookup(userName)
	if err != nil {
		return localUserIdentity{}, fmt.Errorf("重新解析用户 %s 身份失败，保留 ownership 记录：%w", userName, err)
	}
	currentHome := filepath.Clean(current.Home)
	if current.UID != expected.UID || current.GID != expected.GID || currentHome != expected.Home {
		return localUserIdentity{}, fmt.Errorf(
			"用户 %s 身份已变化，拒绝操作新身份并保留 ownership 记录（记录 uid=%d gid=%d home=%s；当前 uid=%d gid=%d home=%s）",
			userName,
			expected.UID,
			expected.GID,
			expected.Home,
			current.UID,
			current.GID,
			currentHome,
		)
	}
	return current, nil
}
