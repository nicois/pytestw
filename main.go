package main

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/fsnotify/fsnotify"
	"github.com/nicois/cache"
	"github.com/nicois/file"
	git "github.com/nicois/git"
	"github.com/nicois/pyast"
	"github.com/nicois/pytestw/pytest"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

/*
run pytest. something like looponfail. also be smart enough to remember failing tests
from previous runs on the same branch
*/

type Watcher interface {
	ClearAndGet() file.Paths
	Set(path string)
}

type watcher struct {
	mutex *sync.Mutex
	paths file.Paths
	adder chan bool
}

func (w *watcher) Set(path string) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.paths.Add(path)
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
	w.paths = make(file.Paths)
}

func (w *watcher) ClearAndGet() file.Paths {
	// waits until at least one is present
	w.mutex.Lock()
	if len(w.paths) == 0 {
		w.mutex.Unlock()
		<-(w.adder)
		w.mutex.Lock()
	}
	defer w.mutex.Unlock()
	result := w.paths
	w.paths = make(file.Paths)
	return result
}

func MakeWatcher(g git.Git) Watcher {
	w := watcher{mutex: new(sync.Mutex), paths: make(file.Paths), adder: make(chan bool)}
	go watch(g, &w)
	return &w
}

func watch(g git.Git, w Watcher) {
	// Create new watcher.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	nWatches := 0
	defer watcher.Close()
	filepath.WalkDir(g.GetRoot(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// skip directories which can't be listened to
			log.Debugf("Not listening for changes in %v: not readable", path)
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			// log.Debugf("Path is %v, name is %v.", path, name)
			if strings.HasPrefix(name, ".") || name == "__pycache__" || g.IsIgnored(path) {
				// log.Debugf("Not listening for changes in %v as it's not tracked by git", path)
				return fs.SkipDir
			}
			err = watcher.Add(path)
			nWatches++
			if err != nil {
				sentry.CaptureException(err)
				log.Fatal(err)
			}
		}
		return nil
	})
	log.Debugf("Watching for changes in %v directories", nWatches)
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Has(fsnotify.Write) {
				if g.IsTracked(event.Name) {
					log.Debugf("%v has changed (and is tracked by git)", event.Name)
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

type DependencyService interface {
	GetDependees(paths file.Paths) (file.Paths, error)
}

func hasTestedAllDependeesRelativeToUpstream(g git.Git, rootPaths []string, testSuite pytest.TestSuite, ds DependencyService) bool {
	upstream := g.GetDefaultUpstream()
	if upstream != "" {
		return hasTestedAllDependees(g.GetChangedPaths(g.GetDefaultUpstream()), rootPaths, testSuite, ds)
	}
	// no changes, so by default they are all tested
	return true
}

func getDependeesForChangesRelativeToUpstream(g git.Git, rootPaths []string, ds DependencyService) file.Paths {
	upstream := g.GetDefaultUpstream()
	if upstream != "" {
		log.Debugf("\tChanged paths relative to upstream: %v", g.GetChangedPaths(g.GetDefaultUpstream()))
		return getDependeesForChanges(g.GetChangedPaths(g.GetDefaultUpstream()), rootPaths, ds)
	}
	// no changes, so no dependees
	return make(file.Paths)
}

func getDependeesForChanges(changedPaths file.Paths, rootPaths []string, ds DependencyService) file.Paths {
	// log.Info("changed paths: ", changedPaths)
	result := file.CreatePaths()
	// only keep a dependee if it sits under one of the root paths
	// TODO: ensure paths are actual valid paths
	// TODO: maybe perform a better check than substring, or at
	// least generate absolute paths first
	deps, err := ds.GetDependees(changedPaths)
	if err != nil {
		log.Panic(err)
	}
	// log.Infof("gd for %v: %v items", changedPaths, len(deps))
outer:
	for candidate := range deps {
		for _, rp := range rootPaths {
			if strings.HasPrefix(candidate, rp) {
				result.Add(candidate)
				continue outer
			}
		}
	}
	return result
}

type Alpha []string

func (a Alpha) Len() int           { return len(a) }
func (a Alpha) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a Alpha) Less(i, j int) bool { return a[i] < a[j] }

// TODO: make generic when F37
func Listify(m file.Paths) []string {
	result := make([]string, len(m))
	i := 0
	for k := range m {
		result[i] = k
		i++
	}
	sort.Sort(Alpha(result))
	return result
}

func hasTestedAllDependees(changes file.Paths, rootPaths []string, testSuite pytest.TestSuite, ds DependencyService) bool {
	if len(rootPaths) == 0 {
		return true
	}
	dependees := getDependeesForChanges(changes, rootPaths, ds)
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

func onBranchChange(g git.Git, v cache.Version) {
	c := make(chan []byte)
	root := g.GetRoot()
	script := filepath.Join(root, "._on_branch_change")
	if file.FileExists(script) {
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
	err := sentry.Init(sentry.ClientOptions{
		Dsn: "https://3db6e318dd094355a2b3f2c589d40998@o4505178845413376.ingest.sentry.io/4505178846396416",
		// Set TracesSampleRate to 1.0 to capture 100%
		// of transactions for performance monitoring.
		// We recommend adjusting this value in production,
		TracesSampleRate: 1.0,
	})
	if err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}
	// Flush buffered events before the program terminates.
	defer sentry.Flush(5 * time.Second)

	g, err := git.Create(".")
	if err != nil {
		log.Warning(err)
	}
	/*
		result := pyast.BuildDependencies(g.GetRoot())
		for d := range result.GetDependencies(file.CreatePaths("/home/nick.farrell/git/aiven-core/tests/unit/prune/services/kafka/test_broker_availability_checker.py")) {
			log.Info(d)
		}
		os.Exit(2)
	*/
	neverRunAllTests := len(os.Getenv("PYTESTW_NEVER_RUN_ALL_TESTS")) > 0

	cacher := cache.Create("pytestw")
	// Look for ./pants, and ready the pants service

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
	branchVersioner := func() cache.Version {
		notify := make(chan []byte)
		go g.DetectBranchChange(notify)
		return cache.CreateListener(notify)
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
	filesChangedSinceLastRun := file.CreatePaths()

	// This is a special sort of Versioner which will provide
	// the current git SHA when asked what the current version is,
	// but will only generate abort messages if the branch changes
	hybrid := cache.CreateHybrid(cache.Reactive(g.GetWorkingHash), branchVersioner)

	// record the pytest switches provided on the commandline
	switches := []string{}

	// record the paths to the tests provided on the commandline
	paths := []string{}

	loopOnFail := false

	// Populate switches and paths from os.Args
	foundPath := false
	for _, arg := range os.Args[1:] {
		if foundPath {
			if path, err := filepath.Abs(arg); err == nil {
				paths = append(paths, path)
			}
			continue
		} else if file.PathExists(arg) {
			foundPath = true
			if path, err := filepath.Abs(arg); err == nil {
				paths = append(paths, path)
			}
		} else {
			if arg == "-f" || arg == "--looponfail" {
				loopOnFail = true
			} else {
				switches = append(switches, arg)
			}
		}
	}
	log.Debugf("Switches: %v; paths: %v", switches, paths)

	if len(paths) == 0 {
		log.Fatal("You have not provided any paths to tests.")
	}

	// Remember which tests were run previously, keyed by branch name
	// - so this is initially an empty map
	testSuites := make(map[string]pytest.TestSuite) // key is branch name

	// Avoid a race condition (where we start running pytest
	// before we have retrieved the current branch name)
	cache.WaitForCurrent(branchVersioner)

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
			testSuite = pytest.TestSuite{}
		}

		// Look at what files have changed relative to upstream. Use this as a basis for
		// working out which python trees should be scanned.
		// (FIXME: when it supports it, we should only recalculate the parts affected by file changes.
		// This is already pretty fast, so redo it each time for now.
		depService := pyast.BuildTrees(pyast.CalculatePythonRoots(g.GetChangedPaths(g.GetDefaultUpstream())), g)

		log.Debugf("Paths: %v", paths)
		log.Debugf("Changes since last run: %q", filesChangedSinceLastRun)
		log.Debugf("Dependees for changes since last run: %q", Listify(getDependeesForChanges(filesChangedSinceLastRun, paths, depService)))
		log.Debugf("Dependees for changes relative to upstream: %q", Listify(getDependeesForChangesRelativeToUpstream(g, paths, depService)))

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
				testSuite, err = pytest.RunTests(g, cacher, switches, failures, hybrid)
			}
		} else if deps := Listify(getDependeesForChanges(filesChangedSinceLastRun, paths, depService)); len(deps) > 0 {
			/*
			   The above line is doing this:
			   - calculate the "dependees" of the git-tracked paths recently changed, according to pants. That is, which files import the files which were edited?
			   - if there is at least one of these "deps", just (re)test these paths.
			*/
			if !firstRun {
				log.Infof("(%v) files have changed recently, so testing their dependencies.", len(filesChangedSinceLastRun))
			}
			testSuite, err = pytest.RunPaths(g, cacher, switches, deps, hybrid)
			lastRunWasFullRun = false
		} else if deps := Listify(getDependeesForChangesRelativeToUpstream(g, paths, depService)); len(deps) > 0 && (lastRunWasFullRun || !hasTestedAllDependeesRelativeToUpstream(g, paths, testSuite, depService)) {
			if firstRun {
				log.Infof("Testing dependencies of local changes relative to %v", g.GetDefaultUpstream())
			} else {
				log.Infof("Previous test run passed but did not test all dependencies of all changed files relative to %v, so testing them.", g.GetDefaultUpstream())
			}
			testSuite, err = pytest.RunPaths(g, cacher, switches, deps, hybrid)
			lastRunWasFullRun = false
		} else if !neverRunAllTests {
			log.Infoln("Running all specified test cases, including tests which should not be related to your changes.")
			testSuite, err = pytest.RunPaths(g, cacher, switches, paths, hybrid)
			lastRunWasFullRun = true
			if testSuite.Errors == 0 && testSuite.Failures == 0 {
				log.Debugln("All tests are passing.")
				waitBeforeTryingAgain = true
			}
		} else {
			log.Debug("Test associated with changed code are passing. Because PYTESTW_NEVER_RUN_ALL_TESTS is set, will not run the entire test suite.")
			waitBeforeTryingAgain = true
			err = nil
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
				filesChangedSinceLastRun = file.CreatePaths()
			}
		}
		firstRun = false
	}
}
