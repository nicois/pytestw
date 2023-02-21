// /usr/bin/true; exec /usr/bin/env go run "$0" "$@"
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sys/unix"
)

/*
run pytest. something like looponfail. also be smart enough to remember failing tests
from previous runs on the same branch
*/

type Watcher interface {
	ClearAndGet() Paths
	Set(path string)
}

type watcher struct {
	mutex *sync.Mutex
	paths Paths
	adder chan bool
}

func (w *watcher) Set(path string) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.paths[path] = Member
	select {
	case w.adder <- true:
	default:
	}
}

func (w *watcher) Reset() {
	// wipes whatever is there, without blocking
	w.mutex.Lock()
	defer w.mutex.Unlock()
	select {
	case <-(w.adder):
	default:
	}
	w.paths = make(Paths)
}

func (w *watcher) ClearAndGet() Paths {
	// waits until at least one is present
	w.mutex.Lock()
	if len(w.paths) == 0 {
		w.mutex.Unlock()
		<-(w.adder)
		w.mutex.Lock()
	}
	defer w.mutex.Unlock()
	result := w.paths
	w.paths = make(Paths)
	return result
}

func MakeWatcher(g Git) Watcher {
	w := watcher{mutex: new(sync.Mutex), paths: make(Paths), adder: make(chan bool)}
	go watch(g, &w)
	return &w
}

func watch(g Git, w Watcher) {
	// Create new watcher.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	filepath.WalkDir(g.GetRoot(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// skip directories which can't be listened to
			log.Debugf("Not listening for changes in %v: not readable", path)
			return nil
		}
		// name := d.Name()
		if d.IsDir() {
			if path == ".git" || g.IsIgnored(path) {
				log.Debugf("Not listening for changes in %v: found in .gitignore", path)
				return fs.SkipDir
			}
			err = watcher.Add(path)
			if err != nil {
				log.Fatal(err)
			}
		}
		return nil
	})
	log.Debugln("Watching for changes...")
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				if !g.IsIgnored(event.Name) {
					log.Debugf("%v has changed (and is not ignored by git)", event.Name)
					w.Set(event.Name)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Error(err)
		}
	}
}

type void struct{}

var member void

func hasTestedAllDependeesRelativeToUpstream(g Git, rootPaths []string, testSuite TestSuite, ps PantsService) bool {
	upstream := g.GetDefaultUpstream()
	if upstream != "" {
		return hasTestedAllDependees(g.GetChangedPaths(g.GetDefaultUpstream()), rootPaths, testSuite, ps)
	}
	// no changes, so by default they are all tested
	return true
}

func getDependeesForChangesRelativeToUpstream(g Git, rootPaths []string, ps PantsService) Paths {
	upstream := g.GetDefaultUpstream()
	if upstream != "" {
		return getDependeesForChanges(g.GetChangedPaths(g.GetDefaultUpstream()), rootPaths, ps)
	}
	// no changes, so no dependees
	return make(Paths)
}

func getDependeesForChanges(changedPaths Paths, rootPaths []string, ps PantsService) Paths {
	result := make(Paths)
	// only keep a dependee if it sits under one of the root paths
	// TODO: ensure paths are actual valid paths
	// TODO: maybe perform a better check than substring, or at
	// least generate absolute paths first
outer:
	for candidate := range ps.Get(changedPaths) {
		for _, rp := range rootPaths {
			if strings.HasPrefix(candidate, rp) {
				result[candidate] = Member
				continue outer
			}
		}
	}
	return result
}

/*
func pants(args []string) ([]byte, error) {
}
*/

type pants struct {
	mutex  *sync.Mutex
	cached map[string]Paths
	cacher Cacher
	// pants does not like being called lots of times in parallel.
	// it is a waste to try more than a few; better to do a few at a time
	sem                *semaphore.Weighted
	semaphore_capacity int64
	git                Git
}

type PantsService interface {
	Get(paths Paths) Paths
}

type mockPantsService struct{}

func (m mockPantsService) Get(paths Paths) Paths {
	return make(Paths)
}

func GetPantsService(g Git, c Cacher) PantsService {
	// Move to the project root
	script := filepath.Join(g.GetRoot(), "pants")

	semaphore_capacity := int64(10)

	if FileExists(script) {
		return &pants{cacher: c, mutex: new(sync.Mutex), cached: make(map[string]Paths), sem: semaphore.NewWeighted(semaphore_capacity), git: g, semaphore_capacity: semaphore_capacity}
	}
	log.Info("./pants could not be found so not using it.")
	return mockPantsService{}
}

func waitTimeout(wg *sync.WaitGroup, timeout time.Duration) bool {
	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()
	select {
	case <-c:
		return false // completed normally
	case <-time.After(timeout):
		return true // timed out
	}
}

func (p *pants) Get(paths Paths) Paths {
	// work out which paths we already have details for
	result := make(Paths)
	missing := make(Paths)
	var wg sync.WaitGroup
	/* use full semaphore capacity for the first pants
	   call, as it is a waste to have multiple instances of this going.
	   In theory subsequent calls should be faster, so let them be parallel.
	*/
	weight := p.semaphore_capacity
	for requested_path := range paths {
		deps, ok := p.cached[requested_path]
		if ok {
			for dep := range deps {
				result[dep] = Member
			}
		} else {
			missing[requested_path] = Member
			wg.Add(1)
			go func(path string, weight int64) {
				defer wg.Done()
				p.sem.Acquire(context.Background(), weight)
				defer p.sem.Release(weight)
				deps := getDependees(p.git, p.cacher, Paths{path: Member})
				p.mutex.Lock()
				defer p.mutex.Unlock()

				p.cached[path] = deps
			}(requested_path, weight)
		}
		weight = 1
	}
	if waitTimeout(&wg, time.Second*5) {
		log.Debugln("After 5 seconds pants is still not finished, so using what I have.")
	}
	p.mutex.Lock()
	defer p.mutex.Unlock()
	for requested_path := range missing {
		deps, ok := p.cached[requested_path]
		if ok {
			for dep := range deps {
				result[dep] = Member
			}
		}
	}

	return result
}

func pantsCall(g Git, args ...string) ([]byte, error) {
	script := filepath.Join(g.GetRoot(), "pants")
	proc := exec.Command(script, args...)
	return proc.CombinedOutput()
}

func getDependees(g Git, c Cacher, p Paths) Paths {
	log.Debugf("Running pants for %v", p)
	args := make([]string, len(p)+1)
	args = append(args, "dependees")
	for path := range p {
		args = append(args, path)
	}
	result := make(Paths)
	hasher := sha256.New()
	for _, arg := range args {
		hasher.Write([]byte(arg))
	}
	// cache the pants dependencies until the branch changes
	output, err := c.Cache(hasher, func(stdout io.Writer, stderr io.Writer, abort chan []byte) ([]byte, error) {
		return pantsCall(g, args...)
	}, Reactive(g.GetBranch))
	if err != nil {
		log.Debug(err) // pants can be slow and unhappy when called lots of times
		// return an empty set
		return result
	}
	for _, path := range strings.Split(string(output), "\n") {
		if strings.HasSuffix(path, ":tests") {
			strippedPath := path[:len(path)-6]
			if FileExists(strippedPath) {
				result[strippedPath] = Member
			}
		}
	}
	log.Debugf("%v", result)
	return result
}

type Alpha []string

func (a Alpha) Len() int           { return len(a) }
func (a Alpha) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a Alpha) Less(i, j int) bool { return a[i] < a[j] }

// TODO: make generic when F37
func Listify(m map[string]Void) []string {
	result := make([]string, len(m))
	i := 0
	for k := range m {
		result[i] = k
		i++
	}
	sort.Sort(Alpha(result))
	return result
}

func hasTestedAllDependees(changes Paths, rootPaths []string, testSuite TestSuite, ps PantsService) bool {
	if len(rootPaths) == 0 {
		return true
	}
	dependees := getDependeesForChanges(changes, rootPaths, ps)
	dependees.Discard(testSuite.Paths)
	if len(dependees) > 0 {
		log.Debugf("Some dependees are not tested: %v", dependees)
	}
	return len(dependees) == 0
}

func ionice(gid, class, level int) error {
	proc := exec.Command("ionice", "-c", fmt.Sprint(class), "-n", fmt.Sprint(level), "-P", fmt.Sprint(gid))
	return proc.Run()
}

func onBranchChange(g Git, v Version) {
	c := make(chan []byte)
	root := g.GetRoot()
	script := filepath.Join(root, "._on_branch_change")
	if FileExists(script) {
		go func() {
			defer v.CancelNotifyOnChange(v.NotifyOnChange([]byte(""), c))
			for {
				<-c
				proc := exec.Command(script)
				proc.Dir = root
				proc.Stdout = os.Stdout
				proc.Stderr = os.Stderr
				proc.Run()
			}
		}()
	}
}

func main() {
	g, err := createGit(".")
	if err != nil {
		log.Warning(err)
	}
	cacher := MakeCacher("pytestw")
	// Look for ./pants, and ready the pants service
	pantsService := GetPantsService(g, cacher)

	// start with debug-level logging if this env var is set
	// TODO: use a proper config package
	if len(os.Getenv("PYTESTW_DEBUG")) > 0 {
		log.SetLevel(log.DebugLevel)
	}
	// This also allows switching between Info and Debug
	// using SIGUSR1 and SIGUSR2
	ControlLogLevelViaSignals()

	// Run 'nice'ly  (nice 10)
	err = unix.Setpriority(unix.PRIO_PGRP, 0, 10)
	if err != nil {
		log.Infoln("Setpriority failed. Not running \"nice\"ly.")
		return
	}
	// Attempt to use ionice to drop the IO priority a little
	ionice(os.Getgid(), 2, 7)

	// Start the service which tracks which branch we're on
	branchVersioner := func() Version {
		notify := make(chan []byte)
		go g.DetectBranchChange(notify)
		return CreateListener(notify)
	}()

	/*
	   undocumented feature: if this script is defined, run it
	   each time the branch changes
	*/
	onBranchChange(g, branchVersioner)

	log.Infof("pytest wrapper: running with %q", os.Args[1:])

	// Start watching the repo for changes to tracked files
	fileChangedWatcher := MakeWatcher(g)

	// At the appropriate times, this variable will be updated
	// to store the files which were written to since the previous run
	filesChangedSinceLastRun := make(Paths)

	// This is a special sort of Versioner which will provide
	// the current git SHA when asked what the current version is,
	// but will only generate abort messages if the branch changes
	hybrid := CreateHybrid(Reactive(g.GetWorkingHash), branchVersioner)

	// record the pytest switches provided on the commandline
	switches := []string{}

	// record the paths to the tests provided on the commandline
	paths := []string{}

	loopOnFail := false

	// Populate switches and paths from os.Args
	foundPath := false
	for _, arg := range os.Args[1:] {
		if foundPath {
			paths = append(paths, arg)
			continue
		} else if PathExists(arg) {
			foundPath = true
			paths = append(paths, arg)
		} else {
			if arg == "-f" || arg == "--looponfail" {
				loopOnFail = true
			} else {
				switches = append(switches, arg)
			}
		}
	}
	log.Debugf("Switches: %v; paths: %v", switches, paths)

	// Remember which tests were run previously, keyed by branch name
	// - so this is initially an empty map
	testSuites := make(map[string]TestSuite) // key is branch name

	// Avoid a race condition (where we start running pytest
	// before we have retrieved the current branch name)
	WaitForCurrent(branchVersioner)

	// what is says on the lid
	firstRun := true

	// true if all possible tests were exercised on the previous run
	lastRunWasFullRun := true

	for {
		// this will be used to double-check the branch didn't
		// change mid-run - in which case we would discard the
		// findings
		branch := string(branchVersioner.Current())
		log.Debugf("At top of loop, on branch %v", branch)

		waitBeforeTryingAgain := false
		testSuite, ok := testSuites[branch]
		if !ok {
			log.Debugf("Using default test suite for branch %v", branch)
			testSuite = TestSuite{}
		}

		/*
		   Here is the main logic: this is where it's decided which tests to run this time
		*/
		if testSuite.Errors > 0 || testSuite.Failures > 0 {
			// easy: there were failures last time, so keep trying them
			log.Infoln("Re-running failing tests.")
			failures := testSuite.GetFailures()
			if len(failures) == 0 {
				delete(testSuites, branch)
				err = fmt.Errorf("Previous test run failed without any actual tests failing")
				continue
			} else {
				testSuite, err = RunTests(g, cacher, switches, failures, hybrid)
			}
		} else if deps := Listify(getDependeesForChanges(filesChangedSinceLastRun, paths, pantsService)); len(deps) > 0 {
			/*
			   The above line is doing this:
			   - calculate the "dependees" of the git-tracked paths recently changed, according to pants. That is, which files import the files which were edited?
			   - if there is at least one of these "deps", just (re)test these paths.
			*/
			if !firstRun {
				log.Infof("(%v) files has changed recently, so testing their dependencies.", len(filesChangedSinceLastRun))
			}
			testSuite, err = RunPaths(g, cacher, switches, deps, hybrid)
			lastRunWasFullRun = false
		} else if deps := Listify(getDependeesForChangesRelativeToUpstream(g, paths, pantsService)); len(deps) > 0 && (lastRunWasFullRun || !hasTestedAllDependeesRelativeToUpstream(g, paths, testSuite, pantsService)) {
			if firstRun {
				log.Infof("Testing dependencies of local changes relative to %v", g.defaultUpstream)
			} else {
				log.Infof("Previous test run passed but did not test all dependencies of all changed files relative to %v, so testing them.", g.defaultUpstream)
			}
			testSuite, err = RunPaths(g, cacher, switches, deps, hybrid)
			lastRunWasFullRun = false
		} else {
			log.Infoln("Running all specified test cases.")
			testSuite, err = RunPaths(g, cacher, switches, paths, hybrid)
			lastRunWasFullRun = true
			if testSuite.Errors == 0 && testSuite.Failures == 0 {
				log.Debugln("All tests are passing.")
				waitBeforeTryingAgain = true
			}
		}

		if !loopOnFail {
			if err != nil {
				log.Error(err)
				os.Exit(1)
			} else if testSuite.Errors > 0 || testSuite.Failures > 0 {
				os.Exit(1)
			} else {
				os.Exit(0)
			}
		}

		if err != nil {
			log.Warn(err)
			// to avoid looping forever for some failures
			filesChangedSinceLastRun.Union(fileChangedWatcher.ClearAndGet())
		} else if testSuite.Errors > 0 {
			// pytest couldn't run the actual tests. Maybe problems with fixtures?
			log.Info("Due to errors, will wait for a file to be changed then will try again.")
			filesChangedSinceLastRun.Union(fileChangedWatcher.ClearAndGet())
		} else {
			newBranch := string(branchVersioner.Current())
			if newBranch == branch {
				log.Debugln("Storing updated test suite")
				testSuites[branch] = testSuite
			}
			if testSuite.Failures > 0 {
				// current tests are failing, so don't do anything until there's a change
				log.Debugf("There are failures (%v/%v), so waiting for changes before trying again.", testSuite.Errors, testSuite.Failures)
				filesChangedSinceLastRun = fileChangedWatcher.ClearAndGet()
			} else if waitBeforeTryingAgain {
				log.Debugln("Waiting for changes.")
				filesChangedSinceLastRun = fileChangedWatcher.ClearAndGet()
			} else {
				filesChangedSinceLastRun = make(Paths)
			}
		}
		firstRun = false
	}
}
