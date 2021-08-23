// monitor.go
//
// This source file is part of the FoundationDB open source project
//
// Copyright 2021 Apple Inc. and the FoundationDB project authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
)

// errorBackoffSeconds is the time to wait after a process fails before starting
// another process.
// This delay will only be applied when there has been more than one failure
// within this time window.
const errorBackoffSeconds = 60

// Monitor provides the main monitor loop
type Monitor struct {
	// ConfigFile defines the path to the config file to load.
	ConfigFile string

	// FDBServerPath defines the path to the fdbserver binary.
	FDBServerPath string

	// ActiveConfiguration defines the active process configuration.
	ActiveConfiguration *ProcessConfiguration

	// ActiveConfigurationBytes defines the source data for the active process
	// configuration.
	ActiveConfigurationBytes []byte

	// LastConfigurationTime is the last time we successfully reloaded the
	// configuration file.
	LastConfigurationTime time.Time

	// ProcessIDs stores the PIDs of the processes that are running. A PID of
	// zero will indicate that a process does not have a run loop. A PID of -1
	// will indicate that a process has a run loop but is not currently running
	// the subprocess.
	ProcessIDs []int

	// Mutex defines a mutex around working with configuration.
	Mutex sync.Mutex

	// PodClient is a client for posting updates about this pod to
	// Kubernetes.
	PodClient *PodClient

	// Logger is the logger instance for this monitor.
	Logger logr.Logger
}

// StartMonitor starts the monitor loop.
func StartMonitor(logger logr.Logger, configFile string, fdbserverPath string) {
	podClient, err := CreatePodClient()
	if err != nil {
		panic(err)
	}

	monitor := &Monitor{
		ConfigFile:    configFile,
		FDBServerPath: fdbserverPath,
		PodClient:     podClient,
		Logger:        logger,
	}

	go func() { monitor.WatchPodTimestamps() }()
	monitor.Run()
}

// LoadConfiguration loads the latest configuration from the config file.
func (monitor *Monitor) LoadConfiguration() {
	file, err := os.Open(monitor.ConfigFile)
	if err != nil {
		monitor.Logger.Error(err, "Error reading monitor config file", "monitorConfigPath", monitor.ConfigFile)
		return
	}
	defer file.Close()
	configuration := &ProcessConfiguration{}
	configurationBytes, err := io.ReadAll(file)
	if err != nil {
		monitor.Logger.Error(err, "Error reading monitor configuration", "monitorConfigPath", monitor.ConfigFile)
	}
	err = json.Unmarshal(configurationBytes, configuration)
	if err != nil {
		monitor.Logger.Error(err, "Error parsing monitor configuration", "rawConfiguration", string(configurationBytes))
		return
	}

	if currentContainerVersion == configuration.Version {
		configuration.BinaryPath = monitor.FDBServerPath
	} else {
		configuration.BinaryPath = path.Join(sharedBinaryDir, configuration.Version, "fdbserver")
	}

	binaryStat, err := os.Stat(configuration.BinaryPath)
	if err != nil {
		monitor.Logger.Error(err, "Error checking binary path for latest configuration", "configuration", configuration, "binaryPath", configuration.BinaryPath)
		return
	}
	if binaryStat.Mode()&0o100 == 0 {
		monitor.Logger.Error(nil, "New binary path is not executable", "configuration", configuration, "binaryPath", configuration.BinaryPath)
		return
	}

	_, err = configuration.GenerateArguments(1, nil)
	if err != nil {
		monitor.Logger.Error(err, "Error generating arguments for latest configuration", "configuration", configuration, "binaryPath", configuration.BinaryPath)
		return
	}

	monitor.Logger.Info("Received new configuration file", "configuration", configuration)
	monitor.Mutex.Lock()
	defer monitor.Mutex.Unlock()

	if monitor.ProcessIDs == nil {
		monitor.ProcessIDs = make([]int, configuration.ServerCount+1)
	} else {
		for len(monitor.ProcessIDs) <= configuration.ServerCount {
			monitor.ProcessIDs = append(monitor.ProcessIDs, 0)
		}
	}

	monitor.ActiveConfiguration = configuration
	monitor.ActiveConfigurationBytes = configurationBytes
	monitor.LastConfigurationTime = time.Now()

	for processNumber := 1; processNumber <= configuration.ServerCount; processNumber++ {
		if monitor.ProcessIDs[processNumber] == 0 {
			monitor.ProcessIDs[processNumber] = -1
			tempNumber := processNumber
			go func() { monitor.RunProcess(tempNumber) }()
		}
	}

	err = monitor.PodClient.UpdateAnnotations(monitor)
	if err != nil {
		monitor.Logger.Error(err, "Error updating pod annotations")
	}
}

// RunProcess runs a loop to continually start and watch a process.
func (monitor *Monitor) RunProcess(processNumber int) {
	pid := 0
	logger := monitor.Logger.WithValues("processNumber", processNumber, "area", "RunProcess")
	logger.Info("Starting run loop")
	for {
		monitor.Mutex.Lock()
		if monitor.ActiveConfiguration.ServerCount < processNumber {
			logger.Info("Terminating run loop")
			monitor.ProcessIDs[processNumber] = 0
			monitor.Mutex.Unlock()
			return
		}
		monitor.Mutex.Unlock()

		arguments, err := monitor.ActiveConfiguration.GenerateArguments(processNumber, nil)
		if err != nil {
			logger.Error(err, "Error generating arguments for subprocess", "configuration", monitor.ActiveConfiguration)
			time.Sleep(errorBackoffSeconds * time.Second)
		}
		cmd := exec.Cmd{
			Path: arguments[0],
			Args: arguments,
		}

		logger.Info("Starting subprocess", "arguments", arguments)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			logger.Error(err, "Error getting stdout from subprocess")
		}

		stderr, err := cmd.StderrPipe()
		if err != nil {
			logger.Error(err, "Error getting stderr from subprocess")
		}

		err = cmd.Start()
		if err != nil {
			logger.Error(err, "Error starting subprocess")
			time.Sleep(errorBackoffSeconds * time.Second)
			continue
		}

		if cmd.Process != nil {
			pid = cmd.Process.Pid
		} else {
			logger.Error(nil, "No Process information availale for subprocess")
		}

		startTime := time.Now()
		logger.Info("Subprocess started", "PID", pid)

		monitor.Mutex.Lock()
		monitor.ProcessIDs[processNumber] = pid
		monitor.Mutex.Unlock()

		if stdout != nil {
			stdoutScanner := bufio.NewScanner(stdout)
			go func() {
				for stdoutScanner.Scan() {
					logger.Info("Subprocess output", "msg", stdoutScanner.Text(), "PID", pid)
				}
			}()
		}

		if stderr != nil {
			stderrScanner := bufio.NewScanner(stderr)
			go func() {
				for stderrScanner.Scan() {
					logger.Error(nil, "Subprocess error log", "msg", stderrScanner.Text(), "PID", pid)
				}
			}()
		}

		err = cmd.Wait()
		if err != nil {
			logger.Error(err, "Error from subprocess", "PID", pid)
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}

		logger.Info("Subprocess terminated", "exitCode", exitCode, "PID", pid)

		endTime := time.Now()
		monitor.Mutex.Lock()
		monitor.ProcessIDs[processNumber] = -1
		monitor.Mutex.Unlock()

		processDuration := endTime.Sub(startTime)
		if processDuration.Seconds() < errorBackoffSeconds {
			logger.Info("Backing off from restarting subprocess", "backOffTimeSeconds", errorBackoffSeconds, "lastExecutionDurationSeconds", processDuration)
			time.Sleep(errorBackoffSeconds * time.Second)
		}
	}
}

// WatchConfiguration detects changes to the monitor configuration file.
func (monitor *Monitor) WatchConfiguration(watcher *fsnotify.Watcher) {
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			monitor.Logger.Info("Detected event on monitor conf file", "event", event)
			if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
				monitor.LoadConfiguration()
			} else if event.Op&fsnotify.Remove == fsnotify.Remove {
				err := watcher.Add(monitor.ConfigFile)
				if err != nil {
					panic(err)
				}
				monitor.LoadConfiguration()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			monitor.Logger.Error(err, "Error watching for file system events")
		}
	}
}

// Run runs the monitor loop.
func (monitor *Monitor) Run() {
	done := make(chan bool, 1)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		latestSignal := <-signals
		monitor.Logger.Info("Received system signal", "signal", latestSignal)
		for processNumber, processID := range monitor.ProcessIDs {
			if processID > 0 {
				subprocessLogger := monitor.Logger.WithValues("processNumber", processNumber, "PID", processID)
				process, err := os.FindProcess(processID)
				if err != nil {
					subprocessLogger.Error(err, "Error finding subprocess")
					continue
				}
				subprocessLogger.Info("Sending signal to subprocess", "signal", latestSignal)
				err = process.Signal(latestSignal)
				if err != nil {
					subprocessLogger.Error(err, "Error signaling subprocess")
					continue
				}
			}
		}
		done <- true
	}()

	monitor.LoadConfiguration()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		panic(err)
	}
	err = watcher.Add(monitor.ConfigFile)
	if err != nil {
		panic(err)
	}

	defer watcher.Close()
	go func() { monitor.WatchConfiguration(watcher) }()

	<-done
}

func (monitor *Monitor) WatchPodTimestamps() {
	for timestamp := range monitor.PodClient.TimestampFeed {
		if timestamp > monitor.LastConfigurationTime.Unix() {
			monitor.LoadConfiguration()
		}
	}
}
