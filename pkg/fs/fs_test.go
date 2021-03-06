/*
Copyright 2013 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package fs

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"camlistore.org/pkg/test"
)

var (
	errmu   sync.Mutex
	lasterr error
)

func condSkip(t *testing.T) {
	errmu.Lock()
	defer errmu.Unlock()
	if lasterr != nil {
		t.Skipf("Skipping test; some other test already failed.")
	}
	if runtime.GOOS != "darwin" {
		t.Skipf("Skipping test on OS %q", runtime.GOOS)
	}
	if runtime.GOOS == "darwin" {
		_, err := os.Stat("/Library/Filesystems/osxfusefs.fs/Support/mount_osxfusefs")
		if os.IsNotExist(err) {
			test.DependencyErrorOrSkip(t)
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

type mountEnv struct {
	t          *testing.T
	mountPoint string
	process    *os.Process
}

func (e *mountEnv) Stat(s *stat) int64 {
	file := filepath.Join(e.mountPoint, ".camli_fs_stats", s.name)
	slurp, err := ioutil.ReadFile(file)
	if err != nil {
		e.t.Fatal(err)
	}
	slurp = bytes.TrimSpace(slurp)
	v, err := strconv.ParseInt(string(slurp), 10, 64)
	if err != nil {
		e.t.Fatalf("unexpected value %q in file %s", slurp, file)
	}
	return v
}

func cammountTest(t *testing.T, fn func(env *mountEnv)) {
	dupLog := io.MultiWriter(os.Stderr, testLog{t})
	log.SetOutput(dupLog)
	defer log.SetOutput(os.Stderr)

	w := test.GetWorld(t)
	mountPoint, err := ioutil.TempDir("", "fs-test-mount")
	if err != nil {
		t.Fatal(err)
	}
	verbose := "false"
	var stderrDest io.Writer = ioutil.Discard
	if v, _ := strconv.ParseBool(os.Getenv("VERBOSE_FUSE")); v {
		verbose = "true"
		stderrDest = testLog{t}
	}
	if v, _ := strconv.ParseBool(os.Getenv("VERBOSE_FUSE_STDERR")); v {
		stderrDest = io.MultiWriter(stderrDest, os.Stderr)
	}

	mount := w.Cmd("cammount", "--debug="+verbose, mountPoint)
	mount.Stderr = stderrDest
	mount.Env = append(mount.Env, "CAMLI_TRACK_FS_STATS=1")

	stdin, err := mount.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := mount.Start(); err != nil {
		t.Fatal(err)
	}
	waitc := make(chan error, 1)
	go func() { waitc <- mount.Wait() }()
	defer func() {
		log.Printf("Sending quit")
		stdin.Write([]byte("q\n"))
		select {
		case <-time.After(5 * time.Second):
			log.Printf("timeout waiting for cammount to finish")
			mount.Process.Kill()
			Unmount(mountPoint)
		case err := <-waitc:
			log.Printf("cammount exited: %v", err)
		}
		if !test.WaitFor(not(dirToBeFUSE(mountPoint)), 5*time.Second, 1*time.Second) {
			// It didn't unmount. Try again.
			Unmount(mountPoint)
		}
	}()

	if !test.WaitFor(dirToBeFUSE(mountPoint), 5*time.Second, 100*time.Millisecond) {
		t.Fatalf("error waiting for %s to be mounted", mountPoint)
	}
	fn(&mountEnv{
		t:          t,
		mountPoint: mountPoint,
		process:    mount.Process,
	})

}

func TestRoot(t *testing.T) {
	condSkip(t)
	cammountTest(t, func(env *mountEnv) {
		f, err := os.Open(env.mountPoint)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		names, err := f.Readdirnames(-1)
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		want := []string{"WELCOME.txt", "date", "recent", "roots", "sha1-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "tag"}
		if !reflect.DeepEqual(names, want) {
			t.Errorf("root directory = %q; want %q", names, want)
		}
	})
}

type testLog struct {
	t *testing.T
}

func (tl testLog) Write(p []byte) (n int, err error) {
	tl.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func TestMutable(t *testing.T) {
	condSkip(t)
	cammountTest(t, func(env *mountEnv) {
		rootDir := filepath.Join(env.mountPoint, "roots", "r")
		if err := os.MkdirAll(rootDir, 0755); err != nil {
			t.Fatalf("Failed to make roots/r dir: %v", err)
		}
		fi, err := os.Stat(rootDir)
		if err != nil || !fi.IsDir() {
			t.Fatalf("Stat of roots/r dir = %v, %v; want a directory", fi, err)
		}

		filename := filepath.Join(rootDir, "x")
		f, err := os.Create(filename)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		fi, err = os.Stat(filename)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != 0 {
			t.Fatalf("Stat of roots/r/x = %v, %v; want a 0-byte regular file", fi, err)
		}

		for _, str := range []string{"foo, ", "bar\n", "another line.\n"} {
			f, err = os.OpenFile(filename, os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				t.Fatalf("OpenFile: %v", err)
			}
			if _, err := f.Write([]byte(str)); err != nil {
				t.Logf("Error with append: %v", err)
				t.Fatalf("Error appending %q to %s: %v", str, filename, err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
		ro0 := env.Stat(mutFileOpenRO)
		slurp, err := ioutil.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if env.Stat(mutFileOpenRO)-ro0 != 1 {
			t.Error("Read didn't trigger read-only path optimization.")
		}

		const want = "foo, bar\nanother line.\n"
		fi, err = os.Stat(filename)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() != int64(len(want)) {
			t.Errorf("Stat of roots/r/x = %v, %v; want a %d byte regular file", fi, len(want), err)
		}
		if got := string(slurp); got != want {
			t.Fatalf("contents = %q; want %q", got, want)
		}

		// Delete it.
		if err := os.Remove(filename); err != nil {
			t.Fatal(err)
		}

		// Gone?
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			t.Fatalf("expected file to be gone; got stat err = %v instead", err)
		}
	})
}

func brokenTest(t *testing.T) {
	if v, _ := strconv.ParseBool(os.Getenv("RUN_BROKEN_TESTS")); !v {
		t.Skipf("Skipping broken tests without RUN_BROKEN_TESTS=1")
	}
}

func TestFinderCopy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("Skipping Darwin-specific test.")
	}
	condSkip(t)
	cammountTest(t, func(env *mountEnv) {
		f, err := ioutil.TempFile("", "finder-copy-file")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(f.Name())
		want := []byte("Some data for Finder to copy.")
		if _, err := f.Write(want); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		destDir := filepath.Join(env.mountPoint, "roots", "r")
		if err := os.MkdirAll(destDir, 0755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("osascript")
		script := fmt.Sprintf(`
tell application "Finder"
  copy file POSIX file %q to folder POSIX file %q
end tell
`, f.Name(), destDir)
		cmd.Stdin = strings.NewReader(script)

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Error running AppleScript: %v, %s", err, out)
		} else {
			t.Logf("AppleScript said: %q", out)
		}

		destFile := filepath.Join(destDir, filepath.Base(f.Name()))
		fi, err := os.Stat(destFile)
		if err != nil {
			t.Errorf("Stat = %v, %v", fi, err)
		}
		if fi.Size() != int64(len(want)) {
			t.Errorf("Dest stat size = %d; want %d", fi.Size(), len(want))
		}
		slurp, err := ioutil.ReadFile(destFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(slurp, want) {
			t.Errorf("Dest file = %q; want %q", slurp, want)
		}
	})
}

func not(cond func() bool) func() bool {
	return func() bool {
		return !cond()
	}
}

func dirToBeFUSE(dir string) func() bool {
	return func() bool {
		out, err := exec.Command("df", dir).CombinedOutput()
		if err != nil {
			return false
		}
		if runtime.GOOS == "darwin" {
			if strings.Contains(string(out), "mount_osxfusefs@") {
				return true
			}
			return false
		}
		return false
	}
}
