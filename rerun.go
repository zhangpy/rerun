// Copyright 2013 The rerun AUTHORS. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"

	"github.com/howeyc/fsnotify"
)

var (
	doTests      bool
	doBuild      bool
	neverRun     bool
	raceDetector bool
	buildTags    string
)

func main() {

	flag.BoolVar(&doTests, "test", false, "Run tests (before running program)")
	flag.BoolVar(&doBuild, "build", false, "Build program")
	flag.StringVar(&buildTags, "build-tags", "", "Build tags")
	flag.BoolVar(&neverRun, "no-run", false, "Do not run")
	flag.BoolVar(&raceDetector, "race", false, "Run program and tests with the race detector")

	flag.Parse()

	if len(flag.Args()) < 1 {
		log.Fatal("Usage: rerun [--test] [--no-run] [--build] [--race] <import path> [arg]*")
	}

	buildpath := flag.Args()[0]
	args := flag.Args()[1:]
	err := rerun(buildpath, args)
	if err != nil {
		logln(err)
	}
}

func install(buildpath, lastError string) (installed bool, errorOutput string, err error) {
	cmdline := []string{"go", "get"}

	if raceDetector {
		cmdline = append(cmdline, "-race")
	}
	cmdline = append(cmdline, buildpath)

	// setup the build command, use a shared buffer for both stdOut and stdErr
	cmd := exec.Command("go", cmdline[1:]...)
	buf := bytes.NewBuffer([]byte{})
	cmd.Stdout = buf
	cmd.Stderr = buf

	err = cmd.Run()

	// when there is any output, the go command failed.
	if buf.Len() > 0 {
		errorOutput = buf.String()
		if errorOutput != lastError {
			fmt.Print(errorOutput)
		}
		err = errors.New("compile error")
		return
	}

	// all seems fine
	return
}

func test(buildpath string) (passed bool, err error) {
	cmdline := []string{"go", "test"}

	if raceDetector {
		cmdline = append(cmdline, "-race")
	}
	cmdline = append(cmdline, "-v", buildpath)

	// setup the build command, use a shared buffer for both stdOut and stdErr
	cmd := exec.Command("go", cmdline[1:]...)
	buf := bytes.NewBuffer([]byte{})
	cmd.Stdout = buf
	cmd.Stderr = buf

	err = cmd.Run()
	passed = err == nil

	if !passed {
		fmt.Println(buf)
	} else {
		logln("tests passed")
	}

	return
}

func gobuild(buildpath string) (passed bool, err error) {
	cmdline := []string{"go", "build"}

	if buildTags != "" {
		cmdline = append(cmdline, "-tags", buildTags)
	}

	if raceDetector {
		cmdline = append(cmdline, "-race")
	}
	cmdline = append(cmdline, "-v", buildpath)

	// setup the build command, use a shared buffer for both stdOut and stdErr
	cmd := exec.Command("go", cmdline[1:]...)
	buf := bytes.NewBuffer([]byte{})
	cmd.Stdout = buf
	cmd.Stderr = buf

	err = cmd.Run()
	passed = err == nil

	if !passed {
		fmt.Println(buf)
	} else {
		logln("build passed")
	}

	return
}

var runch = make(chan bool)

func run(binName, binPath string, args []string) {
	cmdline := append([]string{binName}, args...)
	var proc *os.Process
	restarting := false
	go func() {
		for {
			time.Sleep(time.Second)
			if restarting {
				continue
			}
			if proc == nil {
				logln("process quit, relauch")
				runch <- true
				continue
			}
			ps, err := proc.Wait()
			if err != nil {
				logln("000", err, ps)
			}
			proc = nil
		}
	}()
	for relaunch := range runch {
		logln("launch", binPath)
		restarting = true
		defer func() { restarting = false }()
		if proc != nil {
			err := proc.Signal(os.Interrupt)
			if err != nil {
				logf("error on sending signal to process: '%s', will now hard-kill the process", err)
				proc.Kill()
			}
			proc.Wait()
		}
		if !relaunch {
			continue
		}
		cmd := exec.Command(binPath, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		log.Print(cmdline)
		err := cmd.Start()
		if err != nil {
			logf("error on starting process: '%s'", err)
		}
		proc = cmd.Process
	}
}

func getWatcher(buildpath string) (watcher *fsnotify.Watcher, err error) {
	watcher, err = fsnotify.NewWatcher()
	addToWatcher(watcher, buildpath, map[string]bool{})
	return
}

func addToWatcher(watcher *fsnotify.Watcher, importpath string, watching map[string]bool) {
	pkg, err := build.Import(importpath, "", 0)
	if err != nil {
		return
	}
	if pkg.Goroot {
		return
	}
	watcher.Watch(pkg.Dir)
	watching[importpath] = true
	for _, imp := range pkg.Imports {
		if !watching[imp] {
			addToWatcher(watcher, imp, watching)
		}
	}
}

func rerun(buildpath string, args []string) (err error) {
	logf("setting up %s %v", buildpath, args)

	pkg, err := build.Import(buildpath, "", 0)
	if err != nil {
		return err
	}

	if pkg.Name != "main" {
		return fmt.Errorf("expected package %q, got %q", "main", pkg.Name)
	}

	_, binName := path.Split(buildpath)
	var binPath string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		binPath = filepath.Join(gobin, binName)
	} else {
		binPath = filepath.Join(pkg.BinDir, binName)
	}

	if !(neverRun) {
		go run(binName, binPath, args)
	}

	noRun := false
	if doTests {
		passed, _ := test(buildpath)
		if !passed {
			noRun = true
		}
	}

	if doBuild && !noRun {
		gobuild(buildpath)
	}

	var errorOutput string
	_, errorOutput, ierr := install(buildpath, errorOutput)
	if !noRun && !(neverRun) && ierr == nil {
		runch <- true
	}

	watcher, err := getWatcher(buildpath)
	if err != nil {
		return err
	}

	for {
		// read event from the watcher
		we, _ := <-watcher.Event
		// other files in the directory don't count - we watch the whole thing in case new .go files appear.
		if filepath.Ext(we.Name) != ".go" {
			continue
		}

		logln("change -->", we.Name)

		// close the watcher
		watcher.Close()
		// to clean things up: read events from the watcher until events chan is closed.
		go func(events chan *fsnotify.FileEvent) {
			for range events {

			}
		}(watcher.Event)
		// create a new watcher
		logln("rescanning")
		watcher, err = getWatcher(buildpath)
		if err != nil {
			return
		}

		// we don't need the errors from the new watcher.
		// we continiously discard them from the channel to avoid a deadlock.
		go func(errors chan error) {
			for range errors {

			}
		}(watcher.Error)

		var installed bool
		// rebuild
		installed, errorOutput, _ = install(buildpath, errorOutput)
		if !installed {
			continue
		}

		if doTests {
			passed, _ := test(buildpath)
			if !passed {
				continue
			}
		}

		if doBuild {
			gobuild(buildpath)
		}

		// rerun. if we're only testing, sending
		if !(neverRun) {
			runch <- true
		}
	}
}

func logln(v ...interface{}) {
	var msgs = []interface{}{"[rerun]"}
	msgs = append(msgs, v...)
	log.Println(msgs...)
}

func logf(format string, v ...interface{}) {
	logln(fmt.Sprintf(format, v...))
}
