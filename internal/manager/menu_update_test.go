package manager

import (
	"bufio"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setUpdateMenuInput(t *testing.T, input string) {
	t.Helper()
	old := stdinReader
	stdinReader = bufio.NewReader(strings.NewReader(input))
	t.Cleanup(func() { stdinReader = old })
}

func TestMenuUpdateRoutesWithoutInstallingOnInvalidInput(t *testing.T) {
	setUpdateMenuInput(t, "invalid\n1\n2\n3\n0\n")
	var got []updateOptions
	attempted, err := updateMenuWithRunner(func(options updateOptions) (bool, error) {
		got = append(got, options)
		return false, errors.New("fixture download failed before installation")
	})
	want := []updateOptions{{Check: true, Source: "mirror"}, {Source: "mirror"}, {Source: "github"}}
	if attempted || err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("attempted=%v err=%v routes=%+v", attempted, err, got)
	}
}

func TestMenuUpdateInstallerAttemptAlwaysEndsOldMenu(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{{"success", nil}, {"failure", errors.New("files committed but service initialization failed")}} {
		t.Run(tc.name, func(t *testing.T) {
			setUpdateMenuInput(t, "2\n1\n0\n")
			calls := 0
			attempted, err := updateMenuWithRunner(func(updateOptions) (bool, error) {
				calls++
				return true, tc.err
			})
			if !attempted || calls != 1 || !errors.Is(err, tc.err) {
				t.Fatalf("old process continued: attempted=%v calls=%d err=%v", attempted, calls, err)
			}
		})
	}
}

func TestMenuUpdateCancellationAndEOF(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  error
	}{{"0\n", nil}, {"q\n", nil}, {"", errMenuClosed}, {"2", errMenuClosed}} {
		t.Run(tc.input, func(t *testing.T) {
			setUpdateMenuInput(t, tc.input)
			attempted, err := updateMenuWithRunner(func(updateOptions) (bool, error) {
				t.Fatal("cancelled menu dispatched an update")
				return false, nil
			})
			if attempted || !errors.Is(err, tc.want) {
				t.Fatalf("attempted=%v err=%v, want %v", attempted, err, tc.want)
			}
		})
	}
	for _, actionErr := range []error{errMenuCancelled, errMenuClosed} {
		t.Run(actionErr.Error(), func(t *testing.T) {
			setUpdateMenuInput(t, "2\n1\n0\n")
			calls := 0
			attempted, err := updateMenuWithRunner(func(updateOptions) (bool, error) {
				calls++
				if calls == 1 {
					return false, actionErr
				}
				return false, nil
			})
			if attempted || (actionErr == errMenuClosed && (!errors.Is(err, actionErr) || calls != 1)) || (actionErr == errMenuCancelled && (err != nil || calls != 2)) {
				t.Fatalf("action cancellation mishandled: attempted=%v calls=%d err=%v", attempted, calls, err)
			}
		})
	}
}

func TestMenuUpdateConfirmationCannotInstallOnCancelOrPartialEOF(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  error
	}{{"", errMenuClosed}, {"y", errMenuClosed}, {"yes", errMenuClosed}, {"q\n", errMenuCancelled}, {"\n", nil}, {"n\n", nil}} {
		t.Run(tc.input, func(t *testing.T) {
			setUpdateMenuInput(t, tc.input)
			attempted, err := (&App{}).installPreparedUpdate(updateOptions{}, updateRelease{Tag: "v0.9.3"}, filepath.Join(t.TempDir(), "does-not-exist"), "", "amd64", func(string, Config, string) error {
				t.Fatal("cancelled confirmation invoked installer")
				return nil
			})
			if attempted || !errors.Is(err, tc.want) {
				t.Fatalf("attempted=%v err=%v, want %v", attempted, err, tc.want)
			}
		})
	}
}
