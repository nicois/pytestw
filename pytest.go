package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/frioux/shellquote"

	log "github.com/sirupsen/logrus"
)

type (
	Test  string
	Tests []Test
)

type TestResult struct {
	XMLName    xml.Name    `xml:"testsuites"`
	TestSuites []TestSuite `xml:"testsuite"`
}

type TestSuite struct {
	XMLName   xml.Name   `xml:"testsuite"`
	Name      string     `xml:"name,attr"`
	Errors    int        `xml:"errors,attr"`
	Failures  int        `xml:"failures,attr"`
	Skipped   int        `xml:"skipped,attr"`
	Tests     int        `xml:"tests,attr"`
	TestCases []TestCase `xml:"testcase"`
	Paths     Paths
}

func (t *TestSuite) GetFailures() []TestCase {
	result := make([]TestCase, 0, 10)
	for _, tc := range t.TestCases {
		if len(tc.Failures) > 0 || len(tc.Errors) > 0 {
			if FileExists(tc.Path()) {
				result = append(result, tc)
			}
		}
	}
	return result
}

type TestCase struct {
	XMLName   xml.Name      `xml:"testcase"`
	ClassName string        `xml:"classname,attr"`
	Name      string        `xml:"name,attr"`
	Time      float64       `xml:"time,attr"`
	Failures  []TestFailure `xml:"failure"`
	Errors    []TestError   `xml:"error"`
}

func (t *TestCase) Path() string {
	return ClassToPath(t.ClassName)
}

type TestError struct {
	XMLName xml.Name `xml:"error"`
	Message string   `xml:"message,attr"`
}
type TestFailure struct {
	XMLName   xml.Name `xml:"failure"`
	Message   string   `xml:"message,attr"`
	SystemOut string   `xml:"system-out,attr"`
	SystemErr string   `xml:"system-err,attr"`
}

func CalculateTestCasesFromPath(g Git, c Cacher, path string) []TestCase {
	result := make([]TestCase, 0, 1000)
	hasher := sha256.New()
	if hasher != nil {
		hasher.Write([]byte(path))
	}

	stdout, err := c.Cache(hasher, func(stdout io.Writer, stderr io.Writer, abort chan []byte) ([]byte, error) {
		proc := exec.Command("pytest", "--collect-only", "-q", path)
		result, err := proc.CombinedOutput()
		return result, err
	}, Reactive(g.GetWorkingHash))
	if err != nil {
		log.Fatal(err)
	}
	buff := bufio.NewScanner(bytes.NewReader(stdout))
	for buff.Scan() {
		line := buff.Text()
		if len(line) == 0 {
			// should the buffer be closed first?
			return result
		}
		parts := strings.SplitN(line, "::", 2)
		if len(parts) != 2 {
			log.Fatalf("Expected split by ::, found %v which doesn't split", line)
		}
		result = append(result, TestCase{ClassName: PathToClass(parts[0]), Name: parts[1]})
	}

	return result
}

func PathToClass(path string) string {
	x := strings.ReplaceAll(path, "/", ".")
	return x[:len(x)-3]
}

func ClassToPath(class string) string {
	return strings.ReplaceAll(class, ".", "/") + ".py"
}

func RunPaths(g Git, c Cacher, switches []string, paths []string, v Version) (TestSuite, error) {
	pytestArgs := append(switches, paths...)
	if quoted, err := shellquote.Quote(append([]string{"pytest"}, pytestArgs...)); err == nil {
		log.Info(quoted)
	} else {
		log.Warningf("Something is suspicious about the arguments %q. Running pytest with them anyway: %v", pytestArgs, err)
	}
	testSuite, err := cachedRunPytest(g, c, append(switches, paths...), v)
	testSuite.Paths = CreatePaths(paths...)
	return testSuite, err
}

func cachedRunPytest(g Git, c Cacher, args []string, v Version) (TestSuite, error) {
	hasher := sha256.New()
	if v == nil {
		v = Reactive(g.GetWorkingHash)
	}
	for _, arg := range args {
		hasher.Write([]byte(arg))
	}
	// TODO: split unit/local tests and run them in different processes, then
	// combine the results
	// buildout: remember individual test timings, group tests so the expected time is balanced
	// and run in multiple processes.
	byteValue, _ := c.Cache(hasher, func(stdout io.Writer, stderr io.Writer, abort chan []byte) ([]byte, error) {
		return runPytest(stdout, stderr, abort, args...)
	}, v)
	testResult := TestResult{}
	xml.Unmarshal(byteValue, &testResult)
	if nSuites := len(testResult.TestSuites); nSuites != 1 {
		return TestSuite{}, fmt.Errorf("Expected exactly one testsuite, found %v.\n", nSuites)
	}
	log.Debugf("%v errors, %v failures, %v total", testResult.TestSuites[0].Errors, testResult.TestSuites[0].Failures, testResult.TestSuites[0].Tests)
	return testResult.TestSuites[0], nil
}

func RunTests(g Git, c Cacher, switches []string, tests []TestCase, v Version) (TestSuite, error) {
	args := make([]string, 0, 100)
	paths := make([]string, 0, 100)
	for _, tc := range tests {
		path := ClassToPath(tc.ClassName)
		paths = append(paths, path)
		args = append(args, ClassToPath(tc.ClassName)+"::"+tc.Name)
	}
	testSuite, err := RunPaths(g, c, switches, args, v)
	testSuite.Paths = CreatePaths(paths...)
	return testSuite, err
}

func runPytest(stdout io.Writer, stderr io.Writer, abort chan []byte, args ...string) ([]byte, error) {
	/*
	   Run pytest using the given tests, redirecting stdout/stderr if not nill.
	   If a signal is seen on the `abort` channel, abort pytest immediately.
	*/
	junit_file, err := os.CreateTemp("", "junit*.xml")
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(junit_file.Name())
	args = append([]string{"--junit-xml=" + junit_file.Name(), "--disable-warnings", "--color=yes"}, args...)
	log.Debugf("Running pytest with %q", args)
	ctx := context.Background()
	proc := exec.CommandContext(ctx, "pytest", args...)
	if stdout != nil {
		proc.Stdout = stdout
	}
	if stderr != nil {
		proc.Stderr = stderr
	}
	goodExit := make(chan bool, 2) // used to track whether the goroutine succeeded
	if abort != nil {
		go func(p *exec.Cmd) {
			select {
			case <-goodExit:
				log.Debugln("good exit; not aborted the process")
			case <-abort:
				log.Debugln("bad exit; aborted the process")
				// ctx.Done()
				p.Process.Signal(syscall.SIGKILL)
			}
		}(proc)
	}
	err = proc.Run() // this will often have a nonzero exit code
	if err != nil {
		// only exit if we aborted with a signal. If pytest
		// got a normal error code it's probably OK
		// FIXME: messy
		if fmt.Sprintf("%T", err) == "signal: killed" {
			return nil, err
		}
	}
	goodExit <- true
	close(goodExit)

	xmlFile, err := os.Open(junit_file.Name())
	if err != nil {
		return []byte(""), err
	}
	defer xmlFile.Close()
	result, err := ioutil.ReadAll(xmlFile)
	return result, err
}
