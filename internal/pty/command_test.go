package pty_test

import (
	"reflect"
	"testing"

	"github.com/0xPiranhaCodes/webpty/internal/pty"
)

func TestChildEnvironmentKeepsOnlyAllowlistedVariablesAndSetsTerm(t *testing.T) {
	got := pty.ChildEnvironment([]string{
		"PATH=/bin", "WEBPTY_SECRET=x", "TERM=dumb", "HOME=/home/a", "AWS_SECRET_ACCESS_KEY=k",
		"LC_ALL=C", "LANG=en_US.UTF-8", "SSH_AUTH_SOCK=/tmp/agent", "USER=a", "LOGNAME=a",
		"SHELL=/bin/zsh", "TZ=UTC", "TMPDIR=/tmp", "GITHUB_TOKEN=t", "malformed",
	}, nil)
	want := []string{
		"PATH=/bin", "HOME=/home/a", "LC_ALL=C", "LANG=en_US.UTF-8", "USER=a", "LOGNAME=a",
		"SHELL=/bin/zsh", "TZ=UTC", "TMPDIR=/tmp", "TERM=xterm-256color",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChildEnvironment = %v, want %v", got, want)
	}
}

func TestChildEnvironmentPassesThroughNamedVariablesButNeverWebpty(t *testing.T) {
	got := pty.ChildEnvironment([]string{"SSH_AUTH_SOCK=/tmp/agent", "WEBPTY_SECRET=x", "EDITOR=vi", "OTHER=1"},
		[]string{"SSH_AUTH_SOCK", "WEBPTY_SECRET", "EDITOR"})
	want := []string{"SSH_AUTH_SOCK=/tmp/agent", "EDITOR=vi", "TERM=xterm-256color"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChildEnvironment = %v, want %v", got, want)
	}
}
