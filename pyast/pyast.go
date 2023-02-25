package pyast

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	file "github.com/nicois/pytestw/file"
	"github.com/nicois/pytestw/pytest"
	set "github.com/nicois/pytestw/set"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"
)

type Classes = set.StringSet

func MakeClasses() Classes {
	return Classes(set.Create())
}

type node struct {
	importers Classes
}
type tree struct {
	root  string
	nodes map[string]node // maps class
	mutex *sync.Mutex
}

/*
fixme: use a channel to collect the classes from goroutines
*/

type seen struct {
	nodes Classes
	mutex *sync.Mutex
}

func (s *seen) AddClass(class string) bool {
	// returns true if it was added (ie: not there already)
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if _, ok := s.nodes[class]; ok {
		return false
	}
	s.nodes.Add(class)
	return true
}

func (t *tree) getClassDependencies(wg *sync.WaitGroup, s seen, class string, result chan string) {
	/*
	   If this class has dependencies, emit them out, and schedule recursive
	   checking unless they are already seen
	*/
	defer wg.Done()
	if node, ok := t.nodes[class]; ok {
		result <- class
		for dep := range node.importers {
			if s.AddClass(dep) {
				wg.Add(1)
				go t.getClassDependencies(wg, s, dep, result)
			}
		}
	}
}

func (t *tree) GetDependees(paths file.Paths) (file.Paths, error) {
	/*
		i := 0
		for k, v := range t.nodes {
			log.Infof("%v: %q", k, v)
			i++
			if i > 10 {
				break
			}
		}
	*/
	var wg sync.WaitGroup
	dependees := make(chan string, 10)
	seen := seen{mutex: new(sync.Mutex), nodes: make(Classes)}
	result := file.CreatePaths()
	for path := range paths {
		path, err := filepath.Abs(path)
		if err != nil {
			return result, err
		}
		if !strings.HasPrefix(path, t.root) {
			log.Warningf("%v is not contained within %v. Ignoring it.", path, t.root)
			continue
		}
		class := pytest.PathToClass(path[len(t.root)+1:])
		wg.Add(1)
		go t.getClassDependencies(&wg, seen, class, dependees)
	}
	go func() {
		wg.Wait()
		close(dependees)
	}()
	for class := range dependees {
		if _, ok := result[class]; !ok {
			result.Add(pytest.ClassToPath(t.root, class))
		}
	}
	return result, nil
}

type depPair struct {
	importerClass string
	importedClass string // ie: what is imported by the importer
}

func Build(pythonRoot string) *tree {
	pythonRoot, err := filepath.Abs(pythonRoot)
	if err != nil {
		log.Fatal(err)
	}
	var wg sync.WaitGroup
	depPairs := make(chan depPair)
	wg.Add(1)
	go buildDependencies(&wg, pythonRoot, depPairs)
	go func() {
		wg.Wait()
		close(depPairs)
	}()
	nodes := make(map[string]node)
	numberOfPairs := 0
	for depPair := range depPairs {
		numberOfPairs++
		n, ok := nodes[depPair.importedClass]
		if !ok {
			n = node{importers: MakeClasses()}
			nodes[depPair.importedClass] = n
		}
		n.importers.Add(depPair.importerClass)
	}
	return &tree{root: pythonRoot, nodes: nodes}
}

func buildDependencies(wg *sync.WaitGroup, pythonRoot string, depPairs chan depPair) {
	/*
		f, err := os.Create("cpu.prof")
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close() // error handling omitted for example
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	*/

	// this controls the maximum number of files being read
	// concurrently, to avoid "too many open files" errors
	sem := semaphore.NewWeighted(10)
	filepath.WalkDir(pythonRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Fatalf("Cannot scan %v - not readable", path)
		}
		if d.IsDir() {
			if path != pythonRoot && !file.FileExists(filepath.Join(path, "__init__.py")) {
				log.Debugf("%v does not contain __init__.py so skipping.", path)
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".py") {
			wg.Add(1)
			go scan(wg, depPairs, sem, pythonRoot, path)
		}
		return nil
	})
	wg.Done()
}

func stripComments(source string) string {
	expressions := []*regexp.Regexp{regexp.MustCompile(`(?m)'''.+?'''`), regexp.MustCompile(`(?m)""".+?"""`), regexp.MustCompile(`.+'''`)}
	for _, re := range expressions {
		source = re.ReplaceAllLiteralString(source, "")
	}
	re := regexp.MustCompile(`(?m)(\".*?\"|\'.*?\')|(#[^\r\n]*$)`)
	result := re.ReplaceAllStringFunc(source, func(s string) string {
		if strings.HasPrefix(s, "'") || strings.HasPrefix(s, "\"") {
			return s
		}
		return ""
	})
	return strings.TrimSpace(result)
}

func scan(wg *sync.WaitGroup, depPairs chan depPair, sem *semaphore.Weighted, root string, path string) {
	defer wg.Done()
	if !strings.HasPrefix(path, root) {
		log.Fatalf("%v does not start with %v, so cannot calculate the module path.", path, root)
	}
	class := pytest.PathToClass(path[len(root)+1:])

	reImport := regexp.MustCompile(`(?m)^(?:from[ ]+(\S+)[ ]+)?import[ ]+([^\s\(]+|\([^\)]+?\))(?:[ ]+as[ ]+\S+)?[ ]*$`)
	sem.Acquire(context.Background(), 1)
	content, err := file.ReadBytes(path)
	sem.Release(1)
	if err != nil {
		log.Fatalf("While reading %v: %v", path, err)
	}
	for _, match := range reImport.FindAllStringSubmatch(stripComments(string(content)), -1) {
		log.Debugf("%v ---- %v", match[1], match[2])
		packageName := match[1]
		var names []string
		if strings.Contains(match[2], ",") {
			names = strings.Split(strings.Trim(match[2], "()"), ",")
		} else {
			names = []string{match[2]}
		}
		// FIXME resolve '..'
		if strings.Contains(packageName, "..") {
			log.Fatalf("Cannot yet support .. in import line %v of %v:\n%v", packageName, path, match[0])
		}
		if strings.HasPrefix(packageName, ".") {
			// ie: "from .foo import bar"
			if strings.LastIndex(class, ".") == -1 {
				log.Fatalf("Looking for . in %v (class) with %v (packageName) and %q (match)", class, packageName, match)
			}
			parentClass := class[:strings.LastIndex(class, ".")]
			packageName = parentClass
		}
		// add the parent as a dep, as e.g. "from foo import bar" might mean
		// foo is a module, or foo.bar
		for _, name := range names {
			var dep string
			if packageName == "" {
				dep = strings.TrimSpace(name)
				log.Debugf("'%v' --> '%v'", packageName, dep)
			} else {
				dep = fmt.Sprintf("%v.%v", packageName, strings.TrimSpace(name))
			}
			depPairs <- depPair{importerClass: class, importedClass: dep}
			if lastDotIndex := strings.LastIndex(dep, "."); lastDotIndex > 0 {
				depPairs <- depPair{importerClass: class, importedClass: dep[:lastDotIndex]}
			}
			log.Debugf("'%v' --> '%v'", packageName, dep)
		}

	}
}
