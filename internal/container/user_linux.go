//go:build linux

package container

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const maxIdentityDatabaseBytes = 1 << 20

type containerUserIdentity struct {
	uid           int
	gid           int
	supplementary []int
	home          string
	userName      string
}

// prepareContainerUser resolves the identity inside the container rootfs and
// applies groups/GID while CAP_SETGID is available. The returned function
// performs the final UID switch after requested capability drops are applied.
func prepareContainerUser(spec string, userNamespace bool) (func() error, bool, error) {
	identity, err := resolveContainerUserAt("/", spec)
	if err != nil {
		return nil, false, err
	}
	if userNamespace {
		if identity.uid != 0 || identity.gid != 0 {
			return nil, false, fmt.Errorf("user namespaces currently map only container UID/GID 0")
		}
		return func() error {
			if identity.home != "" {
				return os.Setenv("HOME", identity.home)
			}
			return nil
		}, false, nil
	}

	if os.Geteuid() != 0 {
		if identity.uid != os.Geteuid() || identity.gid != os.Getegid() {
			return nil, false, fmt.Errorf("switching to UID %d/GID %d requires root or a mapped user namespace", identity.uid, identity.gid)
		}
		currentGroups, groupErr := syscall.Getgroups()
		if groupErr != nil {
			return nil, false, fmt.Errorf("inspect current supplementary groups: %w", groupErr)
		}
		sort.Ints(currentGroups)
		expectedGroups := append([]int(nil), identity.supplementary...)
		sort.Ints(expectedGroups)
		if !equalIntSlices(currentGroups, expectedGroups) {
			return nil, false, fmt.Errorf("applying the requested supplementary groups requires CAP_SETGID")
		}
		return func() error {
			if identity.home != "" {
				return os.Setenv("HOME", identity.home)
			}
			return nil
		}, false, nil
	}

	if err := syscall.Setgroups(identity.supplementary); err != nil {
		return nil, false, fmt.Errorf("set supplementary groups: %w", err)
	}
	if identity.gid != os.Getegid() {
		if err := syscall.Setgid(identity.gid); err != nil {
			return nil, false, fmt.Errorf("set primary GID %d: %w", identity.gid, err)
		}
	}
	return func() error {
		if identity.uid != os.Geteuid() {
			if err := syscall.Setuid(identity.uid); err != nil {
				return fmt.Errorf("set UID %d: %w", identity.uid, err)
			}
		}
		home := identity.home
		if home == "" {
			home = "/"
		}
		if err := os.Setenv("HOME", home); err != nil {
			return fmt.Errorf("set HOME: %w", err)
		}
		return nil
	}, identity.uid != 0, nil
}

func resolveContainerUserAt(root, spec string) (containerUserIdentity, error) {
	var identity containerUserIdentity
	if err := validateUserSpec(spec); err != nil {
		return identity, err
	}
	if spec == "" {
		spec = "0"
	}
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	identity.uid, identity.gid = 0, 0
	identity.home = "/root"

	if userPart == "" {
		identity.userName = "root"
	} else if uid, err := parseContainerID(userPart); err == nil {
		identity.uid = uid
		identity.gid = uid
		if entry, found, lookupErr := findPasswdByUID(root, uid); lookupErr != nil {
			return identity, lookupErr
		} else if found {
			identity.userName = entry.name
			identity.gid = entry.gid
			identity.home = entry.home
		}
	} else {
		entry, found, lookupErr := findPasswdByName(root, userPart)
		if lookupErr != nil {
			return identity, lookupErr
		}
		if !found {
			return identity, fmt.Errorf("user %q was not found in /etc/passwd", userPart)
		}
		identity.uid, identity.gid = entry.uid, entry.gid
		identity.userName, identity.home = entry.name, entry.home
	}
	if hasGroup && groupPart != "" {
		if gid, err := parseContainerID(groupPart); err == nil {
			identity.gid = gid
		} else {
			gid, found, lookupErr := findGroupByName(root, groupPart)
			if lookupErr != nil {
				return identity, lookupErr
			}
			if !found {
				return identity, fmt.Errorf("group %q was not found in /etc/group", groupPart)
			}
			identity.gid = gid
		}
	}
	if identity.userName != "" {
		groups, err := supplementaryGroupsForUser(root, identity.userName, identity.gid)
		if err != nil {
			return identity, err
		}
		identity.supplementary = groups
	}
	return identity, nil
}

type passwdEntry struct {
	name string
	uid  int
	gid  int
	home string
}

func findPasswdByName(root, name string) (passwdEntry, bool, error) {
	lines, err := readIdentityDatabase(filepath.Join(root, "etc", "passwd"))
	if err != nil {
		return passwdEntry{}, false, err
	}
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 || fields[0] != name {
			continue
		}
		uid, uidErr := parseContainerID(fields[2])
		gid, gidErr := parseContainerID(fields[3])
		if uidErr != nil || gidErr != nil {
			return passwdEntry{}, false, fmt.Errorf("invalid UID/GID for user %q in /etc/passwd", name)
		}
		return passwdEntry{name: fields[0], uid: uid, gid: gid, home: fields[5]}, true, nil
	}
	return passwdEntry{}, false, nil
}

func findPasswdByUID(root string, uid int) (passwdEntry, bool, error) {
	lines, err := readIdentityDatabase(filepath.Join(root, "etc", "passwd"))
	if err != nil {
		return passwdEntry{}, false, err
	}
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 7 {
			continue
		}
		entryUID, uidErr := parseContainerID(fields[2])
		if uidErr != nil || entryUID != uid {
			continue
		}
		gid, gidErr := parseContainerID(fields[3])
		if gidErr != nil {
			return passwdEntry{}, false, fmt.Errorf("invalid GID for UID %d in /etc/passwd", uid)
		}
		return passwdEntry{name: fields[0], uid: entryUID, gid: gid, home: fields[5]}, true, nil
	}
	return passwdEntry{}, false, nil
}

func findGroupByName(root, name string) (int, bool, error) {
	lines, err := readIdentityDatabase(filepath.Join(root, "etc", "group"))
	if err != nil {
		return 0, false, err
	}
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[0] != name {
			continue
		}
		gid, parseErr := parseContainerID(fields[2])
		if parseErr != nil {
			return 0, false, fmt.Errorf("invalid GID for group %q in /etc/group", name)
		}
		return gid, true, nil
	}
	return 0, false, nil
}

func supplementaryGroupsForUser(root, user string, primaryGID int) ([]int, error) {
	lines, err := readIdentityDatabase(filepath.Join(root, "etc", "group"))
	if err != nil {
		return nil, err
	}
	seen := make(map[int]struct{})
	groups := make([]int, 0)
	for _, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 || fields[3] == "" {
			continue
		}
		gid, parseErr := parseContainerID(fields[2])
		if parseErr != nil || gid == primaryGID {
			continue
		}
		member := false
		for _, name := range strings.Split(fields[3], ",") {
			if name == user {
				member = true
				break
			}
		}
		if !member {
			continue
		}
		if _, exists := seen[gid]; exists {
			continue
		}
		seen[gid] = struct{}{}
		groups = append(groups, gid)
	}
	sort.Ints(groups)
	return groups, nil
}

func readIdentityDatabase(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxIdentityDatabaseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxIdentityDatabaseBytes {
		return nil, fmt.Errorf("%s exceeds the %d-byte identity database limit", path, maxIdentityDatabaseBytes)
	}
	return strings.Split(string(data), "\n"), nil
}

func parseContainerID(value string) (int, error) {
	if value == "" {
		return 0, fmt.Errorf("identity is empty")
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("identity %q is not an unsigned integer", value)
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid numeric identity %q", value)
	}
	return int(parsed), nil
}

func equalIntSlices(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
