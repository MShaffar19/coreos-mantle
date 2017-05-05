// Copyright 2017 CoreOS, Inc.
// Copyright 2009 The Go Authors.
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

package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// H is a type passed to Test functions to manage test state and support formatted test logs.
// Logs are accumulated during execution and dumped to standard output when done.
//
// A test ends when its Test function returns or calls any of the methods
// FailNow, Fatal, Fatalf, SkipNow, Skip, or Skipf. Those methods, as well as
// the Parallel method, must be called only from the goroutine running the
// Test function.
//
// The other reporting methods, such as the variations of Log and Error,
// may be called simultaneously from multiple goroutines.
type H struct {
	mu       sync.RWMutex // guards output, failed, and done.
	output   bytes.Buffer // Output generated by test.
	w        io.Writer    // For flushToParent.
	tap      io.Writer    // Optional TAP log of test results.
	logger   *log.Logger
	ctx      context.Context
	cancel   context.CancelFunc
	ran      bool // Test (or one of its subtests) was executed.
	failed   bool // Test has failed.
	skipped  bool // Test has been skipped.
	finished bool // Test function has completed.
	done     bool // Test is finished and all subtests have completed.
	hasSub   bool

	suite    *Suite
	parent   *H
	level    int       // Nesting depth of test.
	name     string    // Name of test.
	start    time.Time // Time test started
	duration time.Duration
	barrier  chan bool // To signal parallel subtests they may start.
	signal   chan bool // To signal a test is done.
	sub      []*H      // Queue of subtests to be run in parallel.

	isParallel bool
}

func (c *H) parentContext() context.Context {
	if c == nil || c.parent == nil || c.parent.ctx == nil {
		return context.Background()
	}
	return c.parent.ctx
}

// Verbose reports whether the Suite's Verbose option is set.
func (h *H) Verbose() bool {
	return h.suite.opts.Verbose
}

// flushToParent writes c.output to the parent after first writing the header
// with the given format and arguments.
func (c *H) flushToParent(format string, args ...interface{}) {
	p := c.parent
	p.mu.Lock()
	defer p.mu.Unlock()

	fmt.Fprintf(p.w, format, args...)

	// TODO: include test numbers in TAP output.
	if p.tap != nil {
		name := strings.Replace(c.name, "#", "", -1)
		if c.Failed() {
			fmt.Fprintf(p.tap, "not ok - %s\n", name)
		} else if c.Skipped() {
			fmt.Fprintf(p.tap, "ok - %s # SKIP\n", name)
		} else {
			fmt.Fprintf(p.tap, "ok - %s\n", name)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	io.Copy(p.w, &c.output)
}

type indenter struct {
	c *H
}

func (w indenter) Write(b []byte) (n int, err error) {
	n = len(b)
	for len(b) > 0 {
		end := bytes.IndexByte(b, '\n')
		if end == -1 {
			end = len(b)
		} else {
			end++
		}
		// An indent of 4 spaces will neatly align the dashes with the status
		// indicator of the parent.
		const indent = "    "
		w.c.output.WriteString(indent)
		w.c.output.Write(b[:end])
		b = b[end:]
	}
	return
}

// fmtDuration returns a string representing d in the form "87.00s".
func fmtDuration(d time.Duration) string {
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// Name returns the name of the running test or benchmark.
func (c *H) Name() string {
	return c.name
}

// Context returns the context for the current test.
// The context is cancelled when the test finishes.
// A goroutine started during a test can wait for the
// context's Done channel to become readable as a signal that the
// test is over, so that the goroutine can exit.
func (c *H) Context() context.Context {
	return c.ctx
}

func (c *H) setRan() {
	if c.parent != nil {
		c.parent.setRan()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran = true
}

// Fail marks the function as having failed but continues execution.
func (c *H) Fail() {
	if c.parent != nil {
		c.parent.Fail()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// c.done needs to be locked to synchronize checks to c.done in parent tests.
	if c.done {
		panic("Fail in goroutine after " + c.name + " has completed")
	}
	c.failed = true
}

// Failed reports whether the function has failed.
func (c *H) Failed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.failed
}

// FailNow marks the function as having failed and stops its execution.
// Execution will continue at the next test.
// FailNow must be called from the goroutine running the
// test function, not from other goroutines
// created during the test. Calling FailNow does not stop
// those other goroutines.
func (c *H) FailNow() {
	c.Fail()

	// Calling runtime.Goexit will exit the goroutine, which
	// will run the deferred functions in this goroutine,
	// which will eventually run the deferred lines in tRunner,
	// which will signal to the test loop that this test is done.
	//
	// A previous version of this code said:
	//
	//	c.duration = ...
	//	c.signal <- c.self
	//	runtime.Goexit()
	//
	// This previous version duplicated code (those lines are in
	// tRunner no matter what), but worse the goroutine teardown
	// implicit in runtime.Goexit was not guaranteed to complete
	// before the test exited. If a test deferred an important cleanup
	// function (like removing temporary files), there was no guarantee
	// it would run on a test failure. Because we send on c.signal during
	// a top-of-stack deferred function now, we know that the send
	// only happens after any other stacked defers have completed.
	c.finished = true
	runtime.Goexit()
}

// log generates the output. It's always at the same stack depth.
func (c *H) log(s string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logger.Output(3, s)
}

// Log formats its arguments using default formatting, analogous to Println,
// and records the text in the error log. The text will be printed only if
// the test fails or the -harness.v flag is set.
func (c *H) Log(args ...interface{}) { c.log(fmt.Sprintln(args...)) }

// Logf formats its arguments according to the format, analogous to Printf, and
// records the text in the error log. A final newline is added if not provided.
// The text will be printed only if the test fails or the -harness.v flag is set.
func (c *H) Logf(format string, args ...interface{}) { c.log(fmt.Sprintf(format, args...)) }

// Error is equivalent to Log followed by Fail.
func (c *H) Error(args ...interface{}) {
	c.log(fmt.Sprintln(args...))
	c.Fail()
}

// Errorf is equivalent to Logf followed by Fail.
func (c *H) Errorf(format string, args ...interface{}) {
	c.log(fmt.Sprintf(format, args...))
	c.Fail()
}

// Fatal is equivalent to Log followed by FailNow.
func (c *H) Fatal(args ...interface{}) {
	c.log(fmt.Sprintln(args...))
	c.FailNow()
}

// Fatalf is equivalent to Logf followed by FailNow.
func (c *H) Fatalf(format string, args ...interface{}) {
	c.log(fmt.Sprintf(format, args...))
	c.FailNow()
}

// Skip is equivalent to Log followed by SkipNow.
func (c *H) Skip(args ...interface{}) {
	c.log(fmt.Sprintln(args...))
	c.SkipNow()
}

// Skipf is equivalent to Logf followed by SkipNow.
func (c *H) Skipf(format string, args ...interface{}) {
	c.log(fmt.Sprintf(format, args...))
	c.SkipNow()
}

// SkipNow marks the test as having been skipped and stops its execution.
// If a test fails (see Error, Errorf, Fail) and is then skipped,
// it is still considered to have failed.
// Execution will continue at the next test. See also FailNow.
// SkipNow must be called from the goroutine running the test, not from
// other goroutines created during the test. Calling SkipNow does not stop
// those other goroutines.
func (c *H) SkipNow() {
	c.skip()
	c.finished = true
	runtime.Goexit()
}

func (c *H) skip() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.skipped = true
}

// Skipped reports whether the test was skipped.
func (c *H) Skipped() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.skipped
}

func (h *H) mkOutputDir() (dir string, err error) {
	dir = h.suite.outputPath(h.name)
	if err = os.MkdirAll(dir, 0777); err != nil {
		err = fmt.Errorf("Failed to create output dir: %v", err)
	}
	return
}

// OutputDir returns the path to a directory for storing data used by
// the current test. Only test frameworks should care about this.
// Individual tests should normally use H.TempDir or H.TempFile
func (h *H) OutputDir() string {
	dir, err := h.mkOutputDir()
	if err != nil {
		h.log(err.Error())
		h.FailNow()
	}
	return dir
}

// TempDir creates a new directory under OutputDir.
// No cleanup is required.
func (h *H) TempDir(prefix string) string {
	dir, err := h.mkOutputDir()
	if err != nil {
		h.log(err.Error())
		h.FailNow()
	}
	tmp, err := ioutil.TempDir(dir, prefix)
	if err != nil {
		h.log(fmt.Sprintf("Failed to create temp dir: %v", err))
		h.FailNow()
	}
	return tmp
}

// TempFile creates a new file under Outputdir.
// No cleanup is required.
func (h *H) TempFile(prefix string) *os.File {
	dir, err := h.mkOutputDir()
	if err != nil {
		h.log(err.Error())
		h.FailNow()
	}
	tmp, err := ioutil.TempFile(dir, prefix)
	if err != nil {
		h.log(fmt.Sprintf("Failed to create temp file: %v", err))
		h.FailNow()
	}
	return tmp
}

// Parallel signals that this test is to be run in parallel with (and only with)
// other parallel tests.
func (t *H) Parallel() {
	if t.isParallel {
		panic("testing: t.Parallel called multiple times")
	}
	t.isParallel = true

	// We don't want to include the time we spend waiting for serial tests
	// in the test duration. Record the elapsed time thus far and reset the
	// timer afterwards.
	t.duration += time.Since(t.start)

	// Add to the list of tests to be released by the parent.
	t.parent.sub = append(t.parent.sub, t)

	t.signal <- true   // Release calling test.
	<-t.parent.barrier // Wait for the parent test to complete.
	t.suite.waitParallel()
	t.start = time.Now()
}

func tRunner(t *H, fn func(t *H)) {
	t.ctx, t.cancel = context.WithCancel(t.parentContext())
	defer t.cancel()

	// When this goroutine is done, either because fn(t)
	// returned normally or because a test failure triggered
	// a call to runtime.Goexit, record the duration and send
	// a signal saying that the test is done.
	defer func() {
		t.duration += time.Now().Sub(t.start)
		// If the test panicked, print any test output before dying.
		err := recover()
		if !t.finished && err == nil {
			err = fmt.Errorf("test executed panic(nil) or runtime.Goexit")
		}
		if err != nil {
			t.Fail()
			t.report()
			panic(err)
		}

		if len(t.sub) > 0 {
			// Run parallel subtests.
			// Decrease the running count for this test.
			t.suite.release()
			// Release the parallel subtests.
			close(t.barrier)
			// Wait for subtests to complete.
			for _, sub := range t.sub {
				<-sub.signal
			}
			if !t.isParallel {
				// Reacquire the count for sequential tests. See comment in Run.
				t.suite.waitParallel()
			}
		} else if t.isParallel {
			// Only release the count for this test if it was run as a parallel
			// test. See comment in Run method.
			t.suite.release()
		}
		t.report() // Report after all subtests have finished.

		// Do not lock t.done to allow race detector to detect race in case
		// the user does not appropriately synchronizes a goroutine.
		t.done = true
		if t.parent != nil && !t.hasSub {
			t.setRan()
		}
		t.signal <- true
	}()

	t.start = time.Now()
	fn(t)
	t.finished = true
}

// Run runs f as a subtest of t called name. It reports whether f succeeded.
// Run will block until all its parallel subtests have completed.
func (t *H) Run(name string, f func(t *H)) bool {
	t.hasSub = true
	testName, ok := t.suite.match.fullName(t, name)
	if !ok {
		return true
	}
	t = &H{
		barrier: make(chan bool),
		signal:  make(chan bool),
		name:    testName,
		suite:   t.suite,
		parent:  t,
		level:   t.level + 1,
	}
	t.w = indenter{t}
	// Indent logs 8 spaces to distinguish them from sub-test headers.
	const indent = "        "
	t.logger = log.New(&t.output, indent, log.Lshortfile)

	if t.suite.opts.Verbose {
		// Print directly to root's io.Writer so there is no delay.
		root := t.parent
		for ; root.parent != nil; root = root.parent {
		}
		fmt.Fprintf(root.w, "=== RUN   %s\n", t.name)
	}
	// Instead of reducing the running count of this test before calling the
	// tRunner and increasing it afterwards, we rely on tRunner keeping the
	// count correct. This ensures that a sequence of sequential tests runs
	// without being preempted, even when their parent is a parallel test. This
	// may especially reduce surprises if *parallel == 1.
	go tRunner(t, f)
	<-t.signal
	return !t.failed
}

func (t *H) report() {
	if t.parent == nil {
		return
	}
	dstr := fmtDuration(t.duration)
	format := "--- %s: %s (%s)\n"
	if t.Failed() {
		t.flushToParent(format, "FAIL", t.name, dstr)
	} else if t.suite.opts.Verbose {
		if t.Skipped() {
			t.flushToParent(format, "SKIP", t.name, dstr)
		} else {
			t.flushToParent(format, "PASS", t.name, dstr)
		}
	}
}
