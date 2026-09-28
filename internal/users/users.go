// Package users manages the per-app unprivileged system users
// ("dootd-<app>") that app and build processes run as.
package users

import (
	"errors"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
)

// Prefix is prepended to the app name to form the system user name.
const Prefix = "dootd-"

// User is a resolved system user.
type User struct {
	Name string
	UID  uint32
	GID  uint32
}

// NameFor returns the system user name for an app.
func NameFor(app string) string { return Prefix + app }

// Lookup resolves the user for app.
func Lookup(app string) (User, error) {
	u, err := user.Lookup(NameFor(app))
	if err != nil {
		return User{}, fmt.Errorf("users: lookup %s: %w", NameFor(app), err)
	}
	uid, err1 := strconv.ParseUint(u.Uid, 10, 32)
	gid, err2 := strconv.ParseUint(u.Gid, 10, 32)
	if err1 != nil || err2 != nil {
		return User{}, fmt.Errorf("users: %s has non-numeric uid/gid", u.Username)
	}
	if uid == 0 || gid == 0 {
		return User{}, fmt.Errorf("users: refusing to use %s: it has uid or gid 0", u.Username)
	}
	return User{Name: u.Username, UID: uint32(uid), GID: uint32(gid)}, nil
}

// Ensure returns the user for app, creating it (system user, own group, no
// home, no login shell) if it does not exist.
func Ensure(app string) (User, error) {
	u, err := Lookup(app)
	if err == nil {
		return u, nil
	}
	var unknown user.UnknownUserError
	if !errors.As(err, &unknown) {
		return User{}, err
	}
	name := NameFor(app)
	out, err := exec.Command("useradd",
		"--system", "--user-group", "--no-create-home",
		"--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin",
		"--comment", "dootd app "+app, name).CombinedOutput()
	if err != nil {
		return User{}, fmt.Errorf("users: useradd %s: %v: %s", name, err, strings.TrimSpace(string(out)))
	}
	return Lookup(app)
}

// Remove deletes the user and its group. A missing user is not an error.
func Remove(app string) error {
	if _, err := Lookup(app); err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil
		}
		return err
	}
	out, err := exec.Command("userdel", NameFor(app)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("users: userdel %s: %v: %s", NameFor(app), err, strings.TrimSpace(string(out)))
	}
	return nil
}
