// Package systemd embeds dootd's systemd unit so the binary can install
// the exact file that ships in this directory (`dootd setup-host`).
package systemd

import _ "embed"

// Unit is contrib/systemd/dootd.service.
//
//go:embed dootd.service
var Unit string

// UnitPath is where the unit is installed.
const UnitPath = "/etc/systemd/system/dootd.service"
