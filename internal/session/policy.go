package session

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// CommandPolicy restricts which executables sessions may run. A command is
// matched both as given and as resolved through PATH and symlinks, so an
// alias of a denied binary is denied too. Deny wins over Allow; an empty
// allow list permits every command that is not denied.
type CommandPolicy struct {
	allow map[string]bool
	deny  map[string]bool
}

// AllowAllCommands returns the policy a Manager uses when none is configured.
func AllowAllCommands() *CommandPolicy {
	return &CommandPolicy{}
}

// NewCommandPolicy builds a policy from absolute executable paths.
func NewCommandPolicy(allow, deny []string) (*CommandPolicy, error) {
	p := &CommandPolicy{}
	var err error
	if p.allow, err = policySet(allow); err != nil {
		return nil, err
	}
	if p.deny, err = policySet(deny); err != nil {
		return nil, err
	}
	return p, nil
}

func policySet(paths []string) (map[string]bool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	set := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("session: command policy entry %q is not an absolute path", path)
		}
		for _, form := range commandForms(path) {
			set[form] = true
		}
	}
	return set, nil
}

// Permits reports whether path may be run.
func (p *CommandPolicy) Permits(path string) bool {
	if p == nil {
		return true
	}
	forms := commandForms(path)
	for _, form := range forms {
		if p.deny[form] {
			return false
		}
	}
	if p.allow == nil {
		return true
	}
	for _, form := range forms {
		if p.allow[form] {
			return true
		}
	}
	return false
}

// commandForms returns path as given and as it resolves on this host.
func commandForms(path string) []string {
	forms := []string{filepath.Clean(path)}
	if resolved, err := exec.LookPath(path); err == nil {
		forms = append(forms, filepath.Clean(resolved))
		if real, err := filepath.EvalSymlinks(resolved); err == nil {
			forms = append(forms, real)
		}
	} else if real, err := filepath.EvalSymlinks(path); err == nil {
		forms = append(forms, real)
	}
	return forms
}
