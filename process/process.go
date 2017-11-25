package process

// Logic for this file is largely based on:
// https://github.com/jarib/childprocess/blob/783f7a00a1678b5d929062564ef5ae76822dfd62/lib/childprocess/unix/process.rb

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/buildkite/agent/logger"
	"github.com/mattn/go-shellwords"
)

type Process struct {
	Pid        int
	PTY        bool
	Timestamp  bool
	Script     string
	Env        []string
	ExitStatus string

	// For every line in the process output, this callback will be called
	// with the contents of the line if its filter returns true
	LineCallback       func(string)
	LinePreProcessor   func(string) string
	LineCallbackFilter func(string) bool

	// Running is stored as an int32 so we can use atomic operations to
	// set/get it (it's accessed by multiple goroutines)
	running int32

	// The underlying command that is executed
	command *exec.Cmd

	// buffer is a used to buffer output when we are prefixing timestamps
	buffer bytes.Buffer

	// locker for data races on the buffer
	bufferLock sync.Mutex

	// conditions to block on, see See http://openmymind.net/Condition-Variables/
	startedCond *sync.Cond
}

func NewProcess() *Process {
	return &Process{
		startedCond: &sync.Cond{L: &sync.Mutex{}},
	}
}

// If you change header parsing here make sure to change it in the
// buildkite.com frontend logic, too

var headerExpansionRegex = regexp.MustCompile("^(?:\\^\\^\\^\\s+\\+\\+\\+)$")

// Start the process and block until it finishes
func (p *Process) Start() error {
	p.startedCond.L.Lock()

	args, err := shellwords.Parse(p.Script)
	if err != nil {
		return err
	}

	p.command = exec.Command(args[0], args[1:]...)

	// Copy the current processes ENV and merge in the new ones. We do this
	// so the sub process gets PATH and stuff. We merge our path in over
	// the top of the current one so the ENV from Buildkite and the agent
	// take precedence over the agent
	currentEnv := os.Environ()
	p.command.Env = append(currentEnv, p.Env...)

	lineReaderPipe, lineWriterPipe := io.Pipe()

	var waitGroup sync.WaitGroup

	// Toggle between running in a pty
	if p.PTY {
		pty, err := StartPTY(p.command)
		if err != nil {
			p.ExitStatus = "1"
			return err
		}

		p.Pid = p.command.Process.Pid
		p.setRunning(true)

		waitGroup.Add(1)

		go func() {
			logger.Debug("[Process] Starting to copy PTY to the buffer")

			// Copy the pty to our buffer. This will block until it
			// EOF's or something breaks.
			_, err = io.Copy(lineWriterPipe, pty)
			if e, ok := err.(*os.PathError); ok && e.Err == syscall.EIO {
				// We can safely ignore this error, because
				// it's just the PTY telling us that it closed
				// successfully.  See:
				// https://github.com/buildkite/agent/pull/34#issuecomment-46080419
				err = nil
			}

			if err != nil {
				logger.Error("[Process] PTY output copy failed with error: %T: %v", err, err)
			} else {
				logger.Debug("[Process] PTY has finished being copied to the buffer")
			}

			waitGroup.Done()
		}()
	} else {
		p.command.Stdout = lineWriterPipe
		p.command.Stderr = lineWriterPipe
		p.command.Stdin = nil

		err := p.command.Start()
		if err != nil {
			p.ExitStatus = "1"
			return err
		}

		p.Pid = p.command.Process.Pid
		p.setRunning(true)
	}

	logger.Info("[Process] Process is running with PID: %d", p.Pid)

	// Notify other goroutines that are blocked on our Started condition.
	p.startedCond.L.Unlock()
	p.startedCond.Broadcast()

	scanner := bufio.NewScanner(lineReaderPipe)

	var lineCallbackWaitGroup sync.WaitGroup
	waitGroup.Add(1)

	go func() {
		defer waitGroup.Done()

		// We scan line by line so that we can run our various processors, currently this buffers the entire
		// output in memory and then an asynchronous process reads it in chunks
		logger.Debug("[LineScanner] Starting to read lines")
		for scanner.Scan() {
			line := scanner.Text()
			checkedForCallback := false
			lineHasCallback := false
			lineString := p.LinePreProcessor(line)

			// Optionally prefix lines with timestamps
			if p.Timestamp {
				lineHasCallback = p.LineCallbackFilter(lineString)
				checkedForCallback = true

				if lineHasCallback || headerExpansionRegex.MatchString(lineString) {
					// Don't timestamp special lines (e.g. header)
					p.writeOutputBuffer(fmt.Sprintf("%s\n", line))
				} else {
					currentTime := time.Now().UTC().Format(time.RFC3339)
					p.writeOutputBuffer(fmt.Sprintf("[%s] %s\n", currentTime, line))
				}
			} else {
				p.writeOutputBuffer(line + "\n")
			}

			// A callback is an async function that is triggered by a line
			if lineHasCallback || !checkedForCallback {
				lineCallbackWaitGroup.Add(1)
				go func(line string) {
					defer lineCallbackWaitGroup.Done()
					if (checkedForCallback && lineHasCallback) || p.LineCallbackFilter(lineString) {
						p.LineCallback(line)
					}
				}(lineString)
			}
		}

		if err := scanner.Err(); err != nil {
			logger.Debug("[LineScanner] Error from scanner: %v", err)
		}
	}()

	logger.Debug("[LineScanner] Finished")

	// Wait until the process has finished. The returned error is nil if the command runs,
	// has no problems copying stdin, stdout, and stderr, and exits with a zero exit status.
	waitResult := p.command.Wait()

	// Close the line writer pipe
	_ = lineWriterPipe.Close()

	// The process is no longer running at this point
	p.setRunning(false)

	// Find the exit status of the script
	p.ExitStatus = getExitStatus(waitResult)

	logger.Info("Process with PID: %d finished with Exit Status: %s", p.Pid, p.ExitStatus)

	// Sometimes (in docker containers) io.Copy never seems to finish. This is a mega
	// hack around it. If it doesn't finish after 1 second, just continue.
	logger.Debug("[Process] Waiting for routines to finish")
	err = timeoutWait(&waitGroup)
	if err != nil {
		logger.Debug("[Process] Timed out waiting for wait group: (%T: %v)", err, err)
	}

	return nil
}

func (p *Process) writeOutputBuffer(s string) {
	p.bufferLock.Lock()
	defer p.bufferLock.Unlock()
	_, _ = p.buffer.WriteString(s)
}

func (p *Process) Output() string {
	p.bufferLock.Lock()
	defer p.bufferLock.Unlock()
	logger.Debug("[Process] Polling for output: (%d bytes)", p.buffer.Len())
	return p.buffer.String()
}

func (p *Process) Kill() error {
	var err error
	if runtime.GOOS == "windows" {
		// Sending Interrupt on Windows is not implemented.
		// https://golang.org/src/os/exec.go?s=3842:3884#L110
		err = exec.Command("CMD", "/C", "TASKKILL", "/F", "/PID", strconv.Itoa(p.Pid)).Run()
	} else {
		// Send a sigterm
		err = p.signal(syscall.SIGTERM)
	}
	if err != nil {
		return err
	}

	// Make a channel that we'll use as a timeout
	c := make(chan int, 1)
	checking := true

	// Start a routine that checks to see if the process
	// is still alive.
	go func() {
		for checking {
			logger.Debug("[Process] Checking to see if PID: %d is still alive", p.Pid)

			foundProcess, err := os.FindProcess(p.Pid)

			// Can't find the process at all
			if err != nil {
				logger.Debug("[Process] Could not find process with PID: %d", p.Pid)

				break
			}

			// We have some information about the process
			if foundProcess != nil {
				processState, err := foundProcess.Wait()

				if err != nil || processState.Exited() {
					logger.Debug("[Process] Process with PID: %d has exited.", p.Pid)

					break
				}
			}

			// Retry in a moment
			sleepTime := time.Duration(1 * time.Second)
			time.Sleep(sleepTime)
		}

		c <- 1
	}()

	// Timeout this process after 3 seconds
	select {
	case _ = <-c:
		// Was successfully terminated
	case <-time.After(10 * time.Second):
		// Stop checking in the routine above
		checking = false

		// Forcefully kill the thing
		err = p.signal(syscall.SIGKILL)

		if err != nil {
			return err
		}
	}

	return nil
}

func (p *Process) signal(sig os.Signal) error {
	if p.command != nil && p.command.Process != nil {
		logger.Debug("[Process] Sending signal: %s to PID: %d", sig.String(), p.Pid)

		err := p.command.Process.Signal(sig)
		if err != nil {
			logger.Error("[Process] Failed to send signal: %s to PID: %d (%T: %v)", sig.String(), p.Pid, err, err)
			return err
		}
	} else {
		logger.Debug("[Process] No process to signal yet")
	}

	return nil
}

// Returns whether or not the process is running
func (p *Process) IsRunning() bool {
	return atomic.LoadInt32(&p.running) != 0
}

// Sets the running flag of the process
func (p *Process) setRunning(r bool) {
	// Use the atomic package to avoid race conditions when setting the
	// `running` value from multiple routines
	if r {
		atomic.StoreInt32(&p.running, 1)
	} else {
		atomic.StoreInt32(&p.running, 0)
	}
}

// Wait until the process is started, concurrency safe
func (p *Process) WaitStarted() {
	// This pattern is part of sync.Cond: lock, wait, lock
	p.startedCond.L.Lock()
	for !p.IsRunning() {
		p.startedCond.Wait()
	}
	p.startedCond.L.Unlock()
}

// https://github.com/hnakamur/commango/blob/fe42b1cf82bf536ce7e24dceaef6656002e03743/os/executil/executil.go#L29
// TODO: Can this be better?
func getExitStatus(waitResult error) string {
	exitStatus := -1

	if waitResult != nil {
		if err, ok := waitResult.(*exec.ExitError); ok {
			if s, ok := err.Sys().(syscall.WaitStatus); ok {
				exitStatus = s.ExitStatus()
			} else {
				logger.Error("[Process] Unimplemented for system where exec.ExitError.Sys() is not syscall.WaitStatus.")
			}
		} else {
			logger.Error("[Process] Unexpected error type in getExitStatus: %#v", waitResult)
		}
	} else {
		exitStatus = 0
	}

	return fmt.Sprintf("%d", exitStatus)
}

func timeoutWait(waitGroup *sync.WaitGroup) error {
	// Make a chanel that we'll use as a timeout
	c := make(chan int, 1)

	// Start waiting for the routines to finish
	go func() {
		waitGroup.Wait()
		c <- 1
	}()

	select {
	case _ = <-c:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("Timeout")
	}

	return nil
}
