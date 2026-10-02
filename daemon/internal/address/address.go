// Package address parses agent addresses: how an orchestrator names one
// agentmux session, or one thread inside it, across every host in the fleet.
//
//	<instance>@<host>            a session
//	<instance>@<host>#<thread>   a thread inside it (Claude session id, amp
//	                             thread id, opencode session id)
//
// Instance names are unique per host (they are registry file names), so the
// pair is unique across the fleet as long as host names are; see
// hostsconfig.CheckUnique. See docs/design/gateway.md.
package address

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// LocalAlias is the hosts.yaml name meaning "the daemon on this machine".
// It is not fleet-unique, so addresses never use it; Canonical maps it to
// this machine's own host name.
const LocalAlias = "local"

// Address names a session, and optionally a thread inside it.
type Address struct {
	Instance string
	Host     string
	Thread   string // empty for a whole session
}

// instanceRE matches provision's identifier rule for instance names.
var instanceRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// hostRE is one DNS label: what a tailnet machine name looks like.
var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Parse reads "<instance>@<host>[#<thread>]". The host is lower-cased, since
// host names are case-insensitive; the instance and thread keep their case.
func Parse(s string) (Address, error) {
	rest, thread, hasThread := strings.Cut(s, "#")
	instance, host, ok := strings.Cut(rest, "@")
	if !ok {
		return Address{}, fmt.Errorf("address %q: want <instance>@<host>[#<thread>]", s)
	}
	a := Address{Instance: instance, Host: strings.ToLower(host), Thread: thread}
	if !instanceRE.MatchString(a.Instance) {
		return Address{}, fmt.Errorf("address %q: instance must contain only letters, numbers, dots, underscores, and hyphens", s)
	}
	if !hostRE.MatchString(a.Host) {
		return Address{}, fmt.Errorf("address %q: host must be a single host name label (letters, numbers, hyphens)", s)
	}
	if a.Host == LocalAlias {
		return Address{}, fmt.Errorf("address %q: %q is not fleet-unique; use the machine's host name", s, LocalAlias)
	}
	if hasThread && (thread == "" || strings.ContainsAny(thread, " \t\r\n@#")) {
		return Address{}, fmt.Errorf("address %q: thread id must be non-empty with no spaces, @ or #", s)
	}
	return a, nil
}

func (a Address) String() string {
	s := a.Instance + "@" + a.Host
	if a.Thread != "" {
		s += "#" + a.Thread
	}
	return s
}

// Session drops the thread, leaving the session's own address.
func (a Address) Session() Address {
	a.Thread = ""
	return a
}

// Canonical turns a hosts.yaml host name into the name used in addresses:
// lower-cased, and LocalAlias replaced by this machine's short host name.
func Canonical(hostsName string) string {
	if strings.EqualFold(hostsName, LocalAlias) {
		return LocalHostName()
	}
	return strings.ToLower(hostsName)
}

// hostname is os.Hostname, swappable in tests.
var hostname = os.Hostname

// LocalHostName is this machine's host name up to the first dot, lower-cased
// ("build-box.lan" becomes "build-box"), which matches its tailnet
// machine name in the usual setup.
func LocalHostName() string {
	h, err := hostname()
	if err != nil || h == "" {
		return LocalAlias
	}
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}
